// Package spider91migrate 周期性把 spider91 drive 下载到本地的视频
// 上传到一个指定的目标 drive 目录（PikPak、115、123、OneDrive 或 Google Drive），上传成功后：
//
//   - 改写 catalog 行：drive_id / file_id / content_hash 改成目标盘的；
//     视频自身的 id 不变（仍是 spider91-<driveID>-<viewkey>），video_tags、
//     收藏、点赞、views 等关联数据全部保留
//   - 删除本地 mp4（spider91/<id>/videos/<viewkey>.<ext>）和源 thumb
//     （spider91/<id>/thumbs/<viewkey>.jpg）；公共 /p/thumb/<videoID> 副本会保留
//
// 之后回放时，videoSource() 自动落到 /p/stream/<target>/<file_id>，
// proxy 层走对应盘的直链 / 302 直连。
//
// 下次目标盘扫盘时，scanner 通过 (content_hash) / (file_name+size)
// 已有的 findDuplicate 兜底逻辑，不会为同一物理文件再建一行。
package spider91migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/googledrive"
	"github.com/video-site/backend/internal/drives/onedrive"
	"github.com/video-site/backend/internal/drives/p115"
	"github.com/video-site/backend/internal/drives/p123"
	"github.com/video-site/backend/internal/drives/pikpak"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/drives/spider91"
	"github.com/video-site/backend/internal/mediaasset"
)

// uploadTarget 是 migrator 调用目标 drive 的最小接口。任何一种"接收 spider91 上传"的
// 网盘都要实现它；当前 PikPak、115、123、OneDrive 和 Google Drive 各自通过适配器满足。
//
// 这一层抽象把"迁移调用方"和"具体盘的 SDK 协议"解耦：
//   - PikPak 走 GCID + OSS PutObject（pikpak.UploadResult）
//   - 115   走 SHA1   + 秒传 / OSS / 分片（p115.UploadResult）
//   - 123   走 MD5    + 秒传 / S3 预签名分片（p123.UploadResult）
//   - OneDrive 走 SHA1 + 小文件 PUT / 大文件 upload session
//   - Google Drive 走 MD5 + resumable upload session
//
// 各家返回值都被归一成本地的 UploadResult，并在 catalog 改写阶段统一处理。
type uploadTarget interface {
	ID() string
	Kind() string
	RootID() string
	EnsureDir(ctx context.Context, pathFromRoot string) (string, error)
	UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error)
	Rename(ctx context.Context, fileID, newName string) error
}

// Spider91LocalSource is the local source interface used by the migration
// worker. Legacy spider91.Driver and the new scriptcrawler.Driver both satisfy
// it when they are mounted for the Spider91 built-in crawler.
type Spider91LocalSource interface {
	drives.Drive
	VideosDir() string
	ThumbsDir() string
	VideoPath(fileID string) (string, error)
	ThumbPath(fileID string) (string, error)
}

// UploadResult 是 uploadTarget.UploadAndReportHash 的归一返回。
//
// FileID  目标盘上的新文件 ID；
// Hash    GCID（PikPak）、MD5 HEX（123 / Google Drive）或 SHA1 HEX（115 / OneDrive），写入 catalog.content_hash 用于跨盘去重；
// Size    实际上传字节数。
type UploadResult struct {
	FileID string
	Hash   string
	Size   int64
}

const (
	spider91UploadDirName          = "91 Spider"
	scriptCrawlerUploadRootDirName = "Script Crawlers"
)

type migrationPlan struct {
	source             Spider91LocalSource
	row                *catalog.Drive
	sourceKinds        []string
	targetDriveID      string
	target             uploadTarget
	uploadDir          string
	keepLatestN        int
	requireAssetsReady bool
	legacyBackfill     bool
}

// pikpakAdapter / p115Adapter / p123Adapter / onedriveAdapter / googledriveAdapter 把具体 driver 包装成 uploadTarget。
//
// 之所以不让 driver 直接实现 uploadTarget：
//
//  1. 各 driver 的 UploadAndReportXxx 返回的是各自包内的 UploadResult 类型，
//     直接共用同名同签名方法会引入循环依赖；
//  2. driver 包不应该感知 spider91migrate 这一层业务定义。
type pikpakAdapter struct {
	d *pikpak.Driver
}

func (a *pikpakAdapter) ID() string     { return a.d.ID() }
func (a *pikpakAdapter) Kind() string   { return a.d.Kind() }
func (a *pikpakAdapter) RootID() string { return a.d.RootID() }
func (a *pikpakAdapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *pikpakAdapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Hash, Size: res.Size}, nil
}
func (a *pikpakAdapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type p115Adapter struct {
	d *p115.Driver
}

func (a *p115Adapter) ID() string     { return a.d.ID() }
func (a *p115Adapter) Kind() string   { return a.d.Kind() }
func (a *p115Adapter) RootID() string { return a.d.RootID() }
func (a *p115Adapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *p115Adapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportSha1(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Sha1, Size: res.Size}, nil
}
func (a *p115Adapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type p123Adapter struct {
	d *p123.Driver
}

func (a *p123Adapter) ID() string     { return a.d.ID() }
func (a *p123Adapter) Kind() string   { return a.d.Kind() }
func (a *p123Adapter) RootID() string { return a.d.RootID() }
func (a *p123Adapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *p123Adapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Hash, Size: res.Size}, nil
}
func (a *p123Adapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type onedriveAdapter struct {
	d *onedrive.Driver
}

func (a *onedriveAdapter) ID() string     { return a.d.ID() }
func (a *onedriveAdapter) Kind() string   { return a.d.Kind() }
func (a *onedriveAdapter) RootID() string { return a.d.RootID() }
func (a *onedriveAdapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *onedriveAdapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Hash, Size: res.Size}, nil
}
func (a *onedriveAdapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type googledriveAdapter struct {
	d *googledrive.Driver
}

func (a *googledriveAdapter) ID() string     { return a.d.ID() }
func (a *googledriveAdapter) Kind() string   { return a.d.Kind() }
func (a *googledriveAdapter) RootID() string { return a.d.RootID() }
func (a *googledriveAdapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *googledriveAdapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Hash, Size: res.Size}, nil
}
func (a *googledriveAdapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

// adaptUploadTarget 把通用 drive 包装成 uploadTarget。
// 不支持的盘 kind 返回 error；调用方静默跳过。
func adaptUploadTarget(d drives.Drive) (uploadTarget, error) {
	switch v := d.(type) {
	case *pikpak.Driver:
		return &pikpakAdapter{d: v}, nil
	case *p115.Driver:
		return &p115Adapter{d: v}, nil
	case *p123.Driver:
		return &p123Adapter{d: v}, nil
	case *onedrive.Driver:
		return &onedriveAdapter{d: v}, nil
	case *googledrive.Driver:
		return &googledriveAdapter{d: v}, nil
	case uploadTarget:
		// 测试或自定义实现可以直接传入；优先使用具体类型分支以拿到适配器。
		return v, nil
	default:
		return nil, fmt.Errorf("drive %q kind=%s does not support spider91 upload", d.ID(), d.Kind())
	}
}

// Registry 是 worker 用来按 driveID 取 driver 的最小依赖。
type Registry interface {
	Get(id string) (drives.Drive, bool)
	All() []drives.Drive
}

type Config struct {
	Catalog          *catalog.Catalog
	Registry         Registry
	GetTargetDriveID func() string // 通常对应 App.Spider91UploadDriveID()
	// Interval 已废弃 —— 旧版迁移 worker 是周期 ticker，新版只通过 nightly
	// pipeline 调用 RunOnce，不再有内置定时器。保留字段不删是为了兼容外
	// 部 yaml / 测试代码里仍传值的场景。
	Interval   time.Duration
	BatchLimit int // 单轮最多迁多少个，0 时默认 50
	// KeepLatestN 是每个 spider91 drive 在本地保留的最新视频数。
	// 超过的部分中"已迁移"的会被清理；未迁移的不动。0 时默认 15；< 0 关闭清理。
	KeepLatestN int
	// CaptchaCooldown 是迁移 worker 在遇到 PikPak captcha 错误（error_code
	// 4002 / 9）后整体进入冷却的时长。冷却期间 runOnce 直接返回，不再发起任何
	// PikPak API 请求，避免被进一步风控。0 时默认 5 分钟；< 0 关闭冷却（仅用于测试）。
	CaptchaCooldown time.Duration
	CommonThumbDir  string
	OnMigrated      func(videoID string)
}

type Migrator struct {
	cfg     Config
	mu      sync.Mutex
	running bool

	// cooldownMu 保护 cooldownUntil。captcha 冷却的语义：
	//   - migrateDrive 遇到上传失败且 pikpak.IsCaptchaError(err) == true 时
	//     调 setCooldown，未来 cfg.CaptchaCooldown 内 runOnce 直接 noop
	//   - 一次冷却期内只打印一行进入日志和一行恢复日志，避免之前那种
	//     "每秒一条 4002" 的刷屏
	cooldownMu     sync.Mutex
	cooldownUntil  time.Time
	cooldownLogged bool
}

func New(cfg Config) *Migrator {
	if cfg.BatchLimit == 0 {
		cfg.BatchLimit = 50
	}
	if cfg.KeepLatestN == 0 {
		cfg.KeepLatestN = 15
	}
	if cfg.CaptchaCooldown == 0 {
		cfg.CaptchaCooldown = 5 * time.Minute
	}
	return &Migrator{
		cfg: cfg,
	}
}

// inCooldown 返回当前是否处于 captcha 冷却期，以及冷却结束时间。
// 冷却期间应该跳过整个 runOnce —— 不要列盘、不要尝试上传，
// 让 PikPak 喘口气。
func (m *Migrator) inCooldown() (bool, time.Time) {
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	return time.Now().Before(m.cooldownUntil), m.cooldownUntil
}

// cooldownState 返回当前冷却状态。若发现冷却已经过期，会清掉状态并让
// 调用方打印一次恢复日志。
func (m *Migrator) cooldownState() (active bool, until time.Time, resumed bool) {
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	if m.cooldownUntil.IsZero() {
		return false, time.Time{}, false
	}
	until = m.cooldownUntil
	if time.Now().Before(until) {
		return true, until, false
	}
	m.cooldownUntil = time.Time{}
	m.cooldownLogged = false
	return false, until, true
}

// setCooldown 把冷却结束时间往后推 cfg.CaptchaCooldown，并返回结束时间。
// 当 cfg.CaptchaCooldown < 0（仅测试用）时不改任何状态、返回零值。
func (m *Migrator) setCooldown() time.Time {
	if m.cfg.CaptchaCooldown < 0 {
		return time.Time{}
	}
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	m.cooldownUntil = time.Now().Add(m.cfg.CaptchaCooldown)
	m.cooldownLogged = false
	return m.cooldownUntil
}

// markCooldownLogged 是 runOnce 用来只打一次"在冷却中"日志的小工具。
// 第一次返回 false（应该打），第二次起返回 true（不再打），冷却到期 / 重新设置时复位。
func (m *Migrator) markCooldownLogged() bool {
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	if m.cooldownLogged {
		return true
	}
	m.cooldownLogged = true
	return false
}

// Trigger 安排一次"立即跑"。多次调用会被合并成一次（channel buffer=1）。
// RunOnce 跑一次完整迁移：列出所有 spider91 drive，对每个超过 KeepLatestN 的旧
// 视频上传到目标 drive，事务性改写 catalog 行，删本地文件。
//
// 这是上层 nightly 流水线 Phase 3 的入口；不再有周期 ticker / Trigger 通道。
// captcha cooldown 状态在单次 RunOnce 内仍生效（多 drive 时遇到 4002 立即停整轮）；
// 跨调用持久 5 分钟，下次 RunOnce 命中冷却期会直接 noop。
//
// 当前实现不会向调用方返回 error —— 单条迁移失败已在内部记日志并跳过；
// 整轮被 cooldown / context 取消时也通过日志可观测。保留 error 返回签名是为
// 给未来需要把 nightly 失败状态展示给 admin 用。
func (m *Migrator) RunOnce(ctx context.Context) error {
	m.runOnce(ctx)
	return nil
}

// runOnce 单轮：扫所有 spider91 drive，对每条还有本地文件的视频做迁移。
//
// 互斥保证：同一 Migrator 内不会并发跑两轮（避免重复上传）。
func (m *Migrator) runOnce(ctx context.Context) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	// captcha 冷却期间整轮跳过 —— 不做任何 PikPak API 调用、不做本地清理，
	// 等冷却结束。这样从用户视角看：进入冷却 → 一行日志 → 完全静默 → 冷却
	// 结束自然恢复。避免之前每秒一条 4002 的日志雪崩。
	if active, until, resumed := m.cooldownState(); active {
		if !m.markCooldownLogged() {
			log.Printf("[spider91migrate] captcha cooldown active until %s, skipping run", until.Format(time.RFC3339))
		}
		return
	} else if resumed {
		log.Printf("[spider91migrate] captcha cooldown ended at %s, resuming migration", until.Format(time.RFC3339))
	}

	plans := m.migrationPlans(ctx)
	if len(plans) == 0 {
		// 没目标就静默 —— 用户选择了本地保存，或目标盘还没挂载。
		return
	}

	migrated := 0
	backfillTargets := map[string]uploadTarget{}
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return
		}
		n, err := m.migrateDrive(ctx, plan)
		if err != nil {
			log.Printf("[spider91migrate] drive=%s migrate batch error: %v", plan.source.ID(), err)
		}
		migrated += n
		if active, _ := m.inCooldown(); active {
			if migrated > 0 {
				log.Printf("[spider91migrate] migrated %d video(s)", migrated)
			}
			return
		}
		if plan.legacyBackfill {
			backfillTargets[plan.targetDriveID] = plan.target
		}
	}
	if migrated > 0 {
		log.Printf("[spider91migrate] migrated %d video(s)", migrated)
	}

	// 收尾：扫每个本地爬虫 drive 的 videos 目录，把 catalog 已经迁到别处但本地
	// 仍有残留的孤儿文件清掉。这是纯防御性兜底——正常路径下 migrateDrive
	// 已经在迁移成功后立刻 CleanupSpider91Local，不会留孤儿。
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return
		}
		deleted, err := m.cleanupOldLocalVideos(ctx, plan)
		if err != nil {
			log.Printf("[spider91migrate] cleanup drive=%s: %v", plan.source.ID(), err)
		}
		if deleted > 0 {
			log.Printf("[spider91migrate] cleanup drive=%s deleted %d orphan local file(s)", plan.source.ID(), deleted)
		}
	}

	// 回填：把已迁移到 PikPak 的 spider91-* 视频里文件名仍是旧格式
	// （比如刚迁完没改、或人工导入）的统一改成方案 B 期望的格式。
	// 这一步幂等：已经是期望格式的不会再调 Rename。
	for targetDriveID, pp := range backfillTargets {
		if renamed, err := m.backfillFileNames(ctx, targetDriveID, pp); err != nil {
			log.Printf("[spider91migrate] backfill names: %v", err)
		} else if renamed > 0 {
			log.Printf("[spider91migrate] backfilled %d %s file name(s) to desired format", renamed, pp.Kind())
		}
	}
}

// targetKindForLog 把当前目标盘 kind 转成对人友好的简称，用于日志。
// 解析失败时回退 "target"。
func (m *Migrator) targetKindForLog() string {
	if m.cfg.GetTargetDriveID == nil || m.cfg.Registry == nil {
		return "target"
	}
	id := m.cfg.GetTargetDriveID()
	if id == "" {
		return "target"
	}
	d, ok := m.cfg.Registry.Get(id)
	if !ok {
		return "target"
	}
	return d.Kind()
}

// resolveTarget 返回 (target drive ID, target uploadTarget, err)。
// 没设置、drive 找不到，或 drive 类型不支持上传时返回 err（调用方静默跳过）。
func (m *Migrator) resolveTarget() (string, uploadTarget, error) {
	if m.cfg.GetTargetDriveID == nil {
		return "", nil, errors.New("no target getter")
	}
	id := m.cfg.GetTargetDriveID()
	return m.resolveTargetID(id)
}

func (m *Migrator) resolveTargetID(id string) (string, uploadTarget, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", nil, errors.New("target drive not configured")
	}
	if m.cfg.Registry == nil {
		return "", nil, errors.New("registry not configured")
	}
	d, ok := m.cfg.Registry.Get(id)
	if !ok {
		return "", nil, fmt.Errorf("target drive %q not in registry", id)
	}
	t, err := adaptUploadTarget(d)
	if err != nil {
		return "", nil, err
	}
	return id, t, nil
}

func (m *Migrator) migrationPlans(ctx context.Context) []migrationPlan {
	if m == nil || m.cfg.Catalog == nil || m.cfg.Registry == nil {
		return nil
	}
	all := m.cfg.Registry.All()
	out := make([]migrationPlan, 0, len(all))
	for _, d := range all {
		if d == nil {
			continue
		}
		src, ok := d.(Spider91LocalSource)
		if !ok {
			continue
		}
		row, err := m.cfg.Catalog.GetDrive(ctx, d.ID())
		if (err != nil || row == nil) && d.Kind() == spider91.Kind {
			row = &catalog.Drive{ID: d.ID(), Kind: spider91.Kind, RootID: "/"}
		}
		if row == nil {
			continue
		}
		switch row.Kind {
		case scriptcrawler.Kind:
			targetID := strings.TrimSpace(row.Credentials["upload_drive_id"])
			if targetID == "" {
				continue
			}
			resolvedID, target, err := m.resolveTargetID(targetID)
			if err != nil {
				log.Printf("[spider91migrate] crawler=%s upload target=%q unavailable: %v", row.ID, targetID, err)
				continue
			}
			out = append(out, migrationPlan{
				source:             src,
				row:                row,
				sourceKinds:        crawlerSourceKindsForRow(row),
				targetDriveID:      resolvedID,
				target:             target,
				uploadDir:          scriptCrawlerUploadDir(row.ID),
				keepLatestN:        0,
				requireAssetsReady: true,
			})
		case spider91.Kind:
			if m.cfg.GetTargetDriveID == nil {
				continue
			}
			targetID := strings.TrimSpace(m.cfg.GetTargetDriveID())
			if targetID == "" {
				continue
			}
			resolvedID, target, err := m.resolveTargetID(targetID)
			if err != nil {
				continue
			}
			out = append(out, migrationPlan{
				source:         src,
				row:            row,
				sourceKinds:    []string{spider91.Kind},
				targetDriveID:  resolvedID,
				target:         target,
				uploadDir:      spider91UploadDirName,
				keepLatestN:    m.cfg.KeepLatestN,
				legacyBackfill: true,
			})
		}
	}
	return out
}

func crawlerSourceKindsForRow(d *catalog.Drive) []string {
	kinds := []string{scriptcrawler.Kind}
	if d != nil && strings.EqualFold(strings.TrimSpace(d.Credentials["builtin"]), spider91.Kind) {
		kinds = append(kinds, spider91.Kind)
	}
	return kinds
}

func scriptCrawlerUploadDir(driveID string) string {
	driveID = sanitizeUploadDirSegment(driveID)
	if driveID == "" {
		driveID = "crawler"
	}
	return scriptCrawlerUploadRootDirName + "/" + driveID
}

func sanitizeUploadDirSegment(raw string) string {
	clean := sanitizeTitle(raw)
	clean = strings.Trim(clean, "/")
	if clean == "." || clean == ".." {
		return ""
	}
	return clean
}

// spider91Drives 返回当前注册的所有 Spider91 来源本地爬虫 driver。
func (m *Migrator) spider91Drives(ctx context.Context) []Spider91LocalSource {
	all := m.cfg.Registry.All()
	out := make([]Spider91LocalSource, 0, len(all))
	for _, d := range all {
		if !m.isSpider91SourceDrive(ctx, d) {
			continue
		}
		if sd, ok := d.(Spider91LocalSource); ok {
			out = append(out, sd)
		}
	}
	return out
}

func (m *Migrator) isSpider91SourceDrive(ctx context.Context, d drives.Drive) bool {
	if d == nil {
		return false
	}
	if d.Kind() == spider91.Kind {
		return true
	}
	if d.Kind() != scriptcrawler.Kind || m.cfg.Catalog == nil {
		return false
	}
	row, err := m.cfg.Catalog.GetDrive(ctx, d.ID())
	if err != nil || row == nil {
		return false
	}
	if row.Kind == spider91.Kind {
		return true
	}
	return row.Kind == scriptcrawler.Kind && strings.EqualFold(strings.TrimSpace(row.Credentials["builtin"]), spider91.Kind)
}

// migrateDrive 对单个本地爬虫 drive 跑一批迁移；返回成功迁移的条数。
func (m *Migrator) migrateDrive(ctx context.Context, plan migrationPlan) (int, error) {
	src := plan.source
	if src == nil || plan.target == nil || plan.targetDriveID == "" {
		return 0, nil
	}
	keepN := plan.keepLatestN
	if keepN < 0 {
		keepN = 0
	}

	type localFile struct {
		name    string
		modTime time.Time
	}

	entries, err := os.ReadDir(src.VideosDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read videos dir: %w", err)
	}

	files := make([]localFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, localFile{name: e.Name(), modTime: info.ModTime()})
	}

	if plan.keepLatestN >= 0 && len(files) <= keepN {
		return 0, nil
	}

	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })

	skip := keepN
	if plan.keepLatestN < 0 {
		skip = 0
	}
	candidates := files
	if skip < len(files) {
		candidates = files[skip:]
	} else {
		return 0, nil
	}

	localVideos, err := m.cfg.Catalog.ListVideosByDriveID(ctx, src.ID(), 100000)
	if err != nil {
		return 0, fmt.Errorf("list local catalog videos: %w", err)
	}
	byFileID := make(map[string]*catalog.Video, len(localVideos))
	for _, v := range localVideos {
		if v != nil && strings.TrimSpace(v.FileID) != "" {
			byFileID[v.FileID] = v
		}
	}

	migrated := 0
	for _, f := range candidates {
		if err := ctx.Err(); err != nil {
			return migrated, err
		}
		if migrated >= m.cfg.BatchLimit {
			break
		}

		v := m.findVideoForLocalFile(ctx, plan, f.name, byFileID)
		if v == nil {
			continue
		}

		if v.DriveID != src.ID() {
			CleanupSpider91Local(src, f.name)
			continue
		}

		if targetDuplicate, err := m.cfg.Catalog.FindEquivalentVideoOnDrive(ctx, v, plan.targetDriveID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("[spider91migrate] %s find target duplicate: %v", v.ID, err)
			}
		} else if targetDuplicate != nil {
			ok, err := m.bindToExistingTarget(ctx, v, targetDuplicate, plan)
			if err != nil {
				log.Printf("[spider91migrate] %s: %v", v.ID, err)
				continue
			}
			if ok {
				migrated++
				if m.cfg.OnMigrated != nil {
					m.cfg.OnMigrated(v.ID)
				}
			}
			continue
		}

		if plan.requireAssetsReady {
			ready, err := m.crawlerVideoAssetsReady(ctx, v)
			if err != nil {
				log.Printf("[spider91migrate] %s check generated assets: %v", v.ID, err)
				continue
			}
			if !ready {
				continue
			}
		}

		ok, err := m.migrateOne(ctx, v, plan)
		if err != nil {
			log.Printf("[spider91migrate] %s: %v", v.ID, err)
			// captcha 错误（4002 / 9）说明 PikPak 当前正拒绝我们；继续在
			// 同一轮里尝试其它文件大概率会拿到同样的 4002，并且每多一次
			// 失败就多一份"被风控加深"的风险。立即中止当前 batch 并
			// 打开冷却窗口，等 cfg.CaptchaCooldown 之后再重试。
			if pikpak.IsCaptchaError(err) {
				until := m.setCooldown()
				log.Printf("[spider91migrate] drive=%s captcha-blocked, cooling down until %s", src.ID(), until.Format(time.RFC3339))
				return migrated, nil
			}
			continue
		}
		if ok {
			migrated++
			if m.cfg.OnMigrated != nil {
				m.cfg.OnMigrated(v.ID)
			}
		}
	}
	return migrated, nil
}

func (m *Migrator) findVideoForLocalFile(ctx context.Context, plan migrationPlan, localFile string, byFileID map[string]*catalog.Video) *catalog.Video {
	if v := byFileID[localFile]; v != nil {
		return v
	}
	sourceID := stripExt(localFile)
	driveID := ""
	if plan.source != nil {
		driveID = plan.source.ID()
	}
	for _, kind := range plan.sourceKinds {
		id := scriptcrawler.BuildVideoIDForKind(kind, driveID, sourceID)
		v, err := m.cfg.Catalog.GetVideo(ctx, id)
		if err == nil && v != nil {
			return v
		}
	}
	return nil
}

func (m *Migrator) crawlerVideoAssetsReady(ctx context.Context, v *catalog.Video) (bool, error) {
	if v == nil {
		return false, nil
	}
	fingerprintReady := strings.EqualFold(strings.TrimSpace(v.FingerprintStatus), "ready") || strings.TrimSpace(v.SampledSHA256) != ""
	if !fingerprintReady {
		return false, nil
	}
	if strings.EqualFold(strings.TrimSpace(v.PreviewStatus), "ready") {
		return true, nil
	}
	return m.cfg.Catalog.HasReadyEquivalentPreview(ctx, v)
}

// migrateOne 把单条本地爬虫视频上传到目标盘并改写 catalog。
// 返回 (true, nil) 表示真的迁了一条；(false, nil) 表示跳过（本地文件已不在等）；
// (false, err) 表示真出错。
func (m *Migrator) migrateOne(ctx context.Context, v *catalog.Video, plan migrationPlan) (bool, error) {
	src := plan.source
	pp := plan.target
	path, err := src.VideoPath(v.FileID)
	if err != nil {
		return false, fmt.Errorf("resolve local path: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 本地文件被人手动删了，但 catalog 还显示 spider91 drive；
			// 这种状态没法迁移。跳过即可（保留行让管理员可见，避免数据丢失）。
			return false, nil
		}
		return false, fmt.Errorf("stat local: %w", err)
	}
	if info.IsDir() || info.Size() == 0 {
		return false, fmt.Errorf("local file invalid: dir=%v size=%d", info.IsDir(), info.Size())
	}

	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open local: %w", err)
	}
	defer f.Close()

	parent, err := pp.EnsureDir(ctx, plan.uploadDir)
	if err != nil {
		return false, fmt.Errorf("%s ensure %q dir: %w", pp.Kind(), plan.uploadDir, err)
	}
	uploadName := desiredPikPakName(v.Title, sourceIDForUploadName(v, plan), v.Ext)
	res, err := pp.UploadAndReportHash(ctx, parent, uploadName, f, info.Size())
	if err != nil {
		return false, fmt.Errorf("%s upload: %w", pp.Kind(), err)
	}
	if res.FileID == "" {
		return false, fmt.Errorf("%s returned empty file id", pp.Kind())
	}

	// 事务性改写 catalog 行：drive_id / file_id / content_hash
	if err := m.cfg.Catalog.MigrateVideoToDrive(ctx, v.ID, plan.targetDriveID, res.FileID, res.Hash); err != nil {
		return false, fmt.Errorf("catalog migrate: %w", err)
	}
	m.preserveCrawledThumbnail(ctx, src, v)
	// 同步 catalog 里的 file_name，让下次目标盘扫盘时 (file_name, size) 也能匹配上
	if err := m.cfg.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{FileName: uploadName}); err != nil {
		log.Printf("[spider91migrate] %s update file_name after migrate: %v", v.ID, err)
	}

	// 删除本地 mp4 和源 thumb（公共 /p/thumb 副本已在 preserveCrawledThumbnail 中保留）。
	CleanupSpider91Local(src, v.FileID)

	log.Printf("[spider91migrate] %s migrated to drive=%s(kind=%s) file=%s name=%q", v.ID, plan.targetDriveID, pp.Kind(), res.FileID, uploadName)
	return true, nil
}

func (m *Migrator) bindToExistingTarget(ctx context.Context, v, target *catalog.Video, plan migrationPlan) (bool, error) {
	if v == nil || target == nil || plan.source == nil {
		return false, nil
	}
	if plan.targetDriveID == "" || target.FileID == "" {
		return false, nil
	}
	if err := m.cfg.Catalog.MigrateVideoToDrive(ctx, v.ID, plan.targetDriveID, target.FileID, firstNonEmpty(target.ContentHash, v.ContentHash)); err != nil {
		return false, fmt.Errorf("catalog bind existing target: %w", err)
	}
	if target.FileName != "" {
		if err := m.cfg.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{FileName: target.FileName}); err != nil {
			log.Printf("[spider91migrate] %s update file_name after duplicate bind: %v", v.ID, err)
		}
	}
	m.preserveCrawledThumbnail(ctx, plan.source, v)
	CleanupSpider91Local(plan.source, v.FileID)
	log.Printf("[spider91migrate] %s bound to existing drive=%s(kind=%s) file=%s duplicate=%s", v.ID, plan.targetDriveID, plan.target.Kind(), target.FileID, target.ID)
	return true, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sourceIDForUploadName(v *catalog.Video, plan migrationPlan) string {
	if v == nil {
		return ""
	}
	if plan.legacyBackfill {
		return extractViewKey(v.ID)
	}
	for _, kind := range plan.sourceKinds {
		prefix := kind + "-" + plan.source.ID() + "-"
		if strings.HasPrefix(v.ID, prefix) {
			return strings.TrimPrefix(v.ID, prefix)
		}
	}
	if v.FileID != "" {
		return stripExt(v.FileID)
	}
	return extractViewKey(v.ID)
}

func (m *Migrator) preserveCrawledThumbnail(ctx context.Context, src Spider91LocalSource, v *catalog.Video) {
	if m == nil || m.cfg.Catalog == nil || src == nil || v == nil || v.ID == "" || v.FileID == "" {
		return
	}
	commonDir := strings.TrimSpace(m.cfg.CommonThumbDir)
	if commonDir == "" {
		return
	}
	thumbPath, ok := findSpider91ThumbPath(src, v.FileID)
	if !ok {
		if v.ThumbnailURL == "" {
			log.Printf("[spider91migrate] %s crawled thumbnail missing before migration cleanup", v.ID)
		}
		return
	}
	if err := os.MkdirAll(commonDir, 0o755); err != nil {
		log.Printf("[spider91migrate] %s mkdir common thumbs: %v", v.ID, err)
		return
	}
	dst := mediaasset.ThumbnailPathInDir(commonDir, v.ID)
	if _, err := os.Stat(dst); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[spider91migrate] %s stat common thumb: %v", v.ID, err)
			return
		}
		if err := copyFileAtomic(thumbPath, dst); err != nil {
			log.Printf("[spider91migrate] %s preserve crawled thumbnail: %v", v.ID, err)
			return
		}
	}
	if err := m.cfg.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
		ThumbnailURL: "/p/thumb/" + v.ID,
	}); err != nil {
		log.Printf("[spider91migrate] %s update crawled thumbnail url: %v", v.ID, err)
		return
	}
	v.ThumbnailURL = "/p/thumb/" + v.ID
}

func findSpider91ThumbPath(src Spider91LocalSource, fileID string) (string, bool) {
	thumbBase := stripExt(fileID)
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
		thumbPath, err := src.ThumbPath(thumbBase + ext)
		if err != nil {
			continue
		}
		info, statErr := os.Stat(thumbPath)
		if statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return thumbPath, true
		}
	}
	return "", false
}

func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, dst)
}

// CleanupSpider91Local 删除已迁移视频的本地 mp4 和 thumb。
//
// thumb 删除是 best-effort —— 找不到就算了（spider91 thumb 文件名带后缀，
// 我们不知道具体是 .jpg 还是别的，逐个尝试常见后缀）。
//
// 暴露成包级函数方便 cleanup 模块复用（任务 6）。
func CleanupSpider91Local(src Spider91LocalSource, fileID string) {
	videoPath, err := src.VideoPath(fileID)
	if err == nil {
		if err := os.Remove(videoPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[spider91migrate] remove local mp4 %s: %v", videoPath, err)
		}
	}
	// thumb 文件名是 <viewkey>.<ext>；fileID 是 <viewkey>.<videoExt>，
	// 不一定相同。尝试用 fileID 去掉视频扩展名后拼 thumb 常见后缀。
	thumbBase := stripExt(fileID)
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
		thumbPath, err := src.ThumbPath(thumbBase + ext)
		if err != nil {
			continue
		}
		_ = os.Remove(thumbPath) // 忽略错误：找不到很正常
	}
}

func stripExt(name string) string {
	ext := filepath.Ext(name)
	return name[:len(name)-len(ext)]
}

// cleanupOldLocalVideos 是防御性兜底：扫 spider91 drive 本地 videos/ 目录，
// 删除所有 catalog 中已经迁移到别处（drive_id != src.ID()）的本地残留。
//
// 与 migrateDrive 的区别：
//   - 不上传任何东西
//   - 不依赖 KeepLatestN —— 哪怕这个孤儿在"最新 N"窗口内，已迁移就该删
//   - 只看 catalog 状态，不看 mtime
//
// 正常路径下 migrateDrive 迁移成功后立刻 CleanupSpider91Local，所以这里
// 应该不会有任何工作。极端情况（手工改 catalog、迁移过程中 crash）才会
// 找到孤儿。
//
// 返回实际删除的文件个数。
func (m *Migrator) cleanupOldLocalVideos(ctx context.Context, plan migrationPlan) (int, error) {
	src := plan.source
	if src == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(src.VideosDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	deleted := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		if e.IsDir() {
			continue
		}
		v := m.findVideoForLocalFile(ctx, plan, e.Name(), nil)
		if v == nil {
			continue
		}
		if v.DriveID == src.ID() {
			continue
		}
		path, perr := src.VideoPath(e.Name())
		if perr != nil {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[spider91migrate] cleanup remove %s: %v", path, err)
			continue
		}
		// thumb 一并删（best-effort）
		thumbBase := stripExt(e.Name())
		for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
			tp, terr := src.ThumbPath(thumbBase + ext)
			if terr != nil {
				continue
			}
			_ = os.Remove(tp)
		}
		deleted++
	}
	return deleted, nil
}

// backfillFileNames 扫描目标 drive（PikPak、115、123、OneDrive 或 Google Drive）下所有 spider91-* 起始 ID 的视频，
// 对文件名不是 desiredPikPakName(...) 期望格式的，调 target.Rename 修正，
// 并把 catalog.file_name 同步到新名字。
//
// 幂等：已经是期望格式的视频不会触发任何调用。
//
// 返回成功改名的条数。
func (m *Migrator) backfillFileNames(ctx context.Context, targetDriveID string, pp uploadTarget) (int, error) {
	videos, err := m.cfg.Catalog.ListVideosByDriveID(ctx, targetDriveID, 10000)
	if err != nil {
		return 0, fmt.Errorf("list videos: %w", err)
	}
	renamed := 0
	for _, v := range videos {
		if err := ctx.Err(); err != nil {
			return renamed, err
		}
		if !strings.HasPrefix(v.ID, "spider91-") {
			continue
		}
		want := desiredPikPakName(v.Title, extractViewKey(v.ID), v.Ext)
		if v.FileName == want {
			continue
		}
		if v.FileID == "" {
			continue
		}
		if err := pp.Rename(ctx, v.FileID, want); err != nil {
			log.Printf("[spider91migrate] rename %s -> %q: %v", v.ID, want, err)
			continue
		}
		if err := m.cfg.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{FileName: want}); err != nil {
			log.Printf("[spider91migrate] %s update file_name after rename: %v", v.ID, err)
			// 目标盘已经改名成功，但 catalog 更新失败 —— 下轮会重试。继续。
		}
		log.Printf("[spider91migrate] renamed %s on %s: %q -> %q", v.ID, pp.Kind(), v.FileName, want)
		renamed++
	}
	return renamed, nil
}
