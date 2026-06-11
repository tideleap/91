package pikpak

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/video-site/backend/internal/drives"
)

const (
	filesURL       = "https://api-drive.mypikpak.net/drive/v1/files"
	signinURL      = "https://user.mypikpak.net/v1/auth/signin"
	tokenURL       = "https://user.mypikpak.net/v1/auth/token"
	captchaInitURL = "https://user.mypikpak.net/v1/shield/captcha/init"
)

type Driver struct {
	id               string
	rootID           string
	username         string
	password         string
	platform         string
	refreshToken     string
	accessToken      string
	captchaToken     string
	deviceID         string
	userID           string
	disableMediaLink bool

	clientID      string
	clientSecret  string
	clientVersion string
	packageName   string
	algorithms    []string
	userAgent     string

	client          *resty.Client
	onTokenUpdate   func(access, refresh, captcha, deviceID string)
	uploadToOSSFunc func(context.Context, *s3Params, io.Reader) error

	// captchaMu serializes captcha-token refreshes triggered by 4002 / 9
	// recovery in requestOnce. Without it, N concurrent callers all hitting
	// 4002 at once would each post to /v1/shield/captcha/init, racing to
	// overwrite d.captchaToken — wasteful and likely to be flagged by
	// PikPak as abuse. With it, only one refresh is in flight; later
	// callers observe d.captchaToken has changed and skip the refresh.
	captchaMu sync.Mutex

	// listMu / lastListAt / listInterval 做和 p115 driver 一样的列目录限频 +
	// 冷却保护。listMu 保证整个 drive 同一时刻只有一次 list 在跑（避免并发
	// 触发 PikPak 的"操作频繁 error_code=10"）；listInterval 是相邻 list 调用
	// 的最小间隔（默认 1 秒）；命中疑似限流错误时进入 pikpakListCooldown
	// 冷却 10 分钟后再重试，循环直到成功或 ctx 取消。
	listMu       sync.Mutex
	lastListAt   time.Time
	listInterval time.Duration
}

type Config struct {
	ID               string
	Username         string
	Password         string
	Platform         string
	RefreshToken     string
	AccessToken      string
	CaptchaToken     string
	DeviceID         string
	RootID           string
	DisableMediaLink bool
	OnTokenUpdate    func(access, refresh, captcha, deviceID string)
}

func New(c Config) *Driver {
	rootID := strings.TrimSpace(c.RootID)
	if rootID == "0" {
		rootID = ""
	}
	platform := strings.ToLower(strings.TrimSpace(c.Platform))
	if platform == "" {
		platform = "web"
	}
	deviceID := strings.TrimSpace(c.DeviceID)
	if deviceID == "" {
		seed := c.Username + c.Password
		if seed == "" {
			seed = c.ID
		}
		deviceID = md5Hex(seed)
	}
	d := &Driver{
		id:               c.ID,
		rootID:           rootID,
		username:         c.Username,
		password:         c.Password,
		platform:         platform,
		refreshToken:     c.RefreshToken,
		accessToken:      c.AccessToken,
		captchaToken:     c.CaptchaToken,
		deviceID:         deviceID,
		disableMediaLink: c.DisableMediaLink,
		onTokenUpdate:    c.OnTokenUpdate,
		client: resty.New().
			SetTimeout(30*time.Second).
			SetHeader("Accept", "application/json, text/plain, */*"),
		listInterval: 1 * time.Second,
	}
	d.applyPlatformDefaults()
	return d
}

func (d *Driver) Kind() string   { return "pikpak" }
func (d *Driver) ID() string     { return d.id }
func (d *Driver) RootID() string { return d.rootID }

func (d *Driver) Init(ctx context.Context) error {
	clearPersistedCaptcha := func() {
		if d.captchaToken == "" {
			return
		}
		d.captchaToken = ""
		d.persistTokens()
	}

	if d.refreshToken != "" {
		if err := d.refresh(ctx, d.refreshToken); err != nil {
			if !IsCaptchaError(err) || d.username == "" || d.password == "" {
				return err
			}
			clearPersistedCaptcha()
			if err := d.login(ctx); err != nil {
				return fmt.Errorf("pikpak refresh captcha recovery login: %w", err)
			}
		} else {
			// Persisted captcha tokens are short-lived. With a refresh token we can
			// safely request a fresh captcha token after auth, and avoiding the
			// stored value prevents known-stale tokens from poisoning startup.
			clearPersistedCaptcha()
		}
	} else {
		if err := d.login(ctx); err != nil {
			return err
		}
	}
	if err := d.refreshCaptchaTokenAtLogin(ctx, getAction(http.MethodGet, filesURL), d.userID); err != nil {
		return err
	}
	d.persistTokens()
	return nil
}

func (d *Driver) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	if dirID == "" {
		dirID = d.rootID
	}
	files, err := d.listWithRetry(ctx, dirID)
	if err != nil {
		return nil, err
	}
	out := make([]drives.Entry, 0, len(files))
	for _, f := range files {
		out = append(out, fileToEntry(f, dirID))
	}
	return out, nil
}

// pikpakListCooldown 是列目录触发疑似限流错误时的冷却时长。
//
// 与 p115 driver 的 listCooldown 同语义：只要错误属 transient
// （error_code=10 / HTTP 429 / 5xx / 通用 "rate limit" 文本），就持续
// 等 10 分钟再发一次列目录请求，直到成功或 ctx 取消。这样即使 PikPak
// 风控持续较长时间，扫描会自然延后到风控结束，不再丢半棵子树。
const pikpakListCooldown = 10 * time.Minute

func (d *Driver) listWithRetry(ctx context.Context, dirID string) ([]file, error) {
	d.listMu.Lock()
	defer d.listMu.Unlock()

	for attempt := 0; ; attempt++ {
		if err := d.waitForListSlotLocked(ctx); err != nil {
			return nil, err
		}

		files, err := d.getFiles(ctx, dirID)
		if err == nil {
			return files, nil
		}
		// 非 transient 错误（如 cookie 失效、目录不存在）直接返回；继续重试也只会反复失败。
		if !isTransientPikPakListError(err) {
			return nil, err
		}
		log.Printf("[pikpak] list cooling down drive=%s dir=%s cooldown=%s attempt=%d err=%v",
			d.id, dirID, pikpakListCooldown, attempt+1, err)
		if err := pikpakSleepContext(ctx, pikpakListCooldown); err != nil {
			return nil, err
		}
	}
}

// waitForListSlotLocked 节流相邻 list 调用。调用方必须已持有 d.listMu。
func (d *Driver) waitForListSlotLocked(ctx context.Context) error {
	if d.listInterval <= 0 || d.lastListAt.IsZero() {
		d.lastListAt = time.Now()
		return ctx.Err()
	}
	next := d.lastListAt.Add(d.listInterval)
	now := time.Now()
	if now.Before(next) {
		if err := pikpakSleepContext(ctx, next.Sub(now)); err != nil {
			return err
		}
	}
	d.lastListAt = time.Now()
	return ctx.Err()
}

func pikpakSleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isTransientPikPakListError 判断 List 返回的错误是否属"瞬时限频/服务端不可用"
// 类型，需要冷却后重试。覆盖：
//
//   - PikPak 业务码 error_code=10 ("操作频繁"，见 OpenList drivers/pikpak/util.go)
//   - HTTP 429 / 500 / 502 / 503 / 504 / 509（rclone 也把这些归为 retry）
//   - 通用文本：rate limit / too many requests / blocked / temporarily unavailable
//
// 不包含 4122/4121/16（access_token 过期）和 9/4002（captcha 过期）—— 这些
// 由 requestOnce 内部已经做过一次自动恢复重试；如果恢复后仍然报这类错误，
// 大概率是凭证或账号本身有问题，继续冷却重试无意义。
func isTransientPikPakListError(err error) bool {
	if err == nil {
		return false
	}
	// 命中 PikPak 业务错误对象
	var apiErr *errResp
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode {
		case 10: // 操作频繁
			return true
		}
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "error_code=10") ||
		strings.Contains(text, "429") ||
		strings.Contains(text, "http 500") ||
		strings.Contains(text, "http 502") ||
		strings.Contains(text, "http 503") ||
		strings.Contains(text, "http 504") ||
		strings.Contains(text, "http 509") ||
		strings.Contains(text, "too many request") ||
		strings.Contains(text, "too many requests") ||
		strings.Contains(text, "rate limit") ||
		strings.Contains(text, "operation frequent") ||
		strings.Contains(text, "操作频繁") ||
		strings.Contains(text, "blocked") ||
		strings.Contains(text, "temporarily unavailable") ||
		strings.Contains(text, "service unavailable")
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
	var f file
	err := d.request(ctx, filesURL+"/"+fileID, http.MethodGet, func(req *resty.Request) {
		req.SetQueryParams(map[string]string{
			"_magic":         "2021",
			"usage":          "FETCH",
			"thumbnail_size": "SIZE_LARGE",
		})
	}, &f)
	if err != nil {
		return nil, fmt.Errorf("pikpak stat: %w", err)
	}
	e := fileToEntry(f, "")
	return &e, nil
}

func (d *Driver) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	var f file
	usage := "FETCH"
	if !d.disableMediaLink {
		usage = "CACHE"
	}
	err := d.request(ctx, filesURL+"/"+fileID, http.MethodGet, func(req *resty.Request) {
		req.SetQueryParams(map[string]string{
			"_magic":         "2021",
			"usage":          usage,
			"thumbnail_size": "SIZE_LARGE",
		})
	}, &f)
	if err != nil {
		return nil, fmt.Errorf("pikpak download url: %w", err)
	}

	url := f.WebContentLink
	expires := time.Now().Add(10 * time.Minute)
	if !d.disableMediaLink {
		if m, ok := pickMediaLink(f.Medias); ok {
			url = m.Link.URL
			if !m.Link.Expire.IsZero() {
				expires = m.Link.Expire
			}
		}
	}
	if url == "" {
		return nil, errors.New("pikpak download url: empty")
	}
	headers := http.Header{}
	if d.userAgent != "" {
		headers.Set("User-Agent", d.userAgent)
	}
	return &drives.StreamLink{
		URL:     url,
		Headers: headers,
		Expires: expires,
	}, nil
}

// Upload 的真正实现见 upload.go。

// Rename 把 fileID 这个文件改名为 newName（不能是空字符串）。
// PikPak API：PATCH /drive/v1/files/<id> 带 body {"name": newName}。
// 与 OpenList drivers/pikpak/driver.go 的 Rename 行为一致。
func (d *Driver) Rename(ctx context.Context, fileID, newName string) error {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return errors.New("pikpak rename: empty file id")
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return errors.New("pikpak rename: empty new name")
	}
	if err := d.request(ctx, filesURL+"/"+fileID, http.MethodPatch, func(req *resty.Request) {
		req.SetBody(map[string]any{"name": newName})
	}, nil); err != nil {
		return fmt.Errorf("pikpak rename: %w", err)
	}
	return nil
}

func (d *Driver) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	currentID := d.rootID
	for _, name := range splitPath(pathFromRoot) {
		childID, err := d.findChildDir(ctx, currentID, name)
		if err != nil {
			return "", err
		}
		if childID == "" {
			childID, err = d.makeDir(ctx, currentID, name)
			if err != nil {
				return "", err
			}
		}
		currentID = childID
	}
	return currentID, nil
}

func (d *Driver) findChildDir(ctx context.Context, parentID, name string) (string, error) {
	entries, err := d.List(ctx, parentID)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir && e.Name == name {
			return e.ID, nil
		}
	}
	return "", nil
}

func (d *Driver) makeDir(ctx context.Context, parentID, name string) (string, error) {
	var out file
	err := d.request(ctx, filesURL, http.MethodPost, func(req *resty.Request) {
		req.SetBody(map[string]any{
			"kind":      "drive#folder",
			"parent_id": parentID,
			"name":      name,
		})
	}, &out)
	if err != nil {
		return "", fmt.Errorf("pikpak mkdir %s: %w", name, err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("pikpak mkdir %s: empty folder id", name)
	}
	return out.ID, nil
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func (d *Driver) getFiles(ctx context.Context, parentID string) ([]file, error) {
	out := make([]file, 0)
	pageToken := "first"
	for pageToken != "" {
		if pageToken == "first" {
			pageToken = ""
		}
		query := map[string]string{
			"parent_id":      parentID,
			"thumbnail_size": "SIZE_LARGE",
			"with_audit":     "true",
			"limit":          "100",
			"filters":        `{"phase":{"eq":"PHASE_TYPE_COMPLETE"},"trashed":{"eq":false}}`,
			"page_token":     pageToken,
		}
		var resp filesResp
		if err := d.request(ctx, filesURL, http.MethodGet, func(req *resty.Request) {
			req.SetQueryParams(query)
		}, &resp); err != nil {
			return nil, fmt.Errorf("pikpak list: %w", err)
		}
		out = append(out, resp.Files...)
		pageToken = resp.NextPageToken
	}
	return out, nil
}

func (d *Driver) request(ctx context.Context, url, method string, configure func(*resty.Request), out any) error {
	return d.requestOnce(ctx, url, method, configure, out, true)
}

func (d *Driver) requestOnce(ctx context.Context, url, method string, configure func(*resty.Request), out any, retry bool) error {
	req := d.client.R().
		SetContext(ctx).
		SetHeader("User-Agent", d.userAgent).
		SetHeader("X-Device-ID", d.deviceID).
		SetHeader("X-Captcha-Token", d.captchaToken)
	if d.accessToken != "" {
		req.SetHeader("Authorization", "Bearer "+d.accessToken)
	}
	if configure != nil {
		configure(req)
	}
	if out != nil {
		req.SetResult(out)
	}
	var e errResp
	req.SetError(&e)

	res, err := req.Execute(method, url)
	if err != nil {
		return err
	}
	if e.isError() {
		switch e.ErrorCode {
		case 4122, 4121, 16:
			if retry {
				if err := d.refresh(ctx, d.refreshToken); err != nil {
					return err
				}
				return d.requestOnce(ctx, url, method, configure, out, false)
			}
		case 9, 4002:
			if retry {
				// Snapshot the token we *just used* (which the server rejected).
				// Then take captchaMu so concurrent recovery attempts are
				// serialized. Once we hold the lock, if d.captchaToken has
				// already moved past staleToken, another goroutine has refreshed
				// it for us — we skip the refresh and just retry. Otherwise we
				// clear the cached token before asking /v1/shield/captcha/init
				// for a fresh one. PikPak may report stale captcha as either
				// 4002 or 9, and sending the rejected token into captcha init can
				// keep returning captcha_invalid.
				staleToken := d.captchaToken
				d.captchaMu.Lock()
				var refreshErr error
				if d.captchaToken == staleToken {
					if d.captchaToken != "" {
						d.captchaToken = ""
					}
					refreshErr = d.refreshCaptchaTokenAtLogin(ctx, getAction(method, url), d.userID)
				}
				d.captchaMu.Unlock()
				if refreshErr != nil {
					return refreshErr
				}
				return d.requestOnce(ctx, url, method, configure, out, false)
			}
		}
		return &e
	}
	if res.IsError() {
		return fmt.Errorf("pikpak http %d: %s", res.StatusCode(), string(res.Body()))
	}
	return nil
}

func pickMediaLink(items []media) (media, bool) {
	if len(items) == 0 {
		return media{}, false
	}
	for _, m := range items {
		if m.IsOrigin && m.Link.URL != "" {
			return m, true
		}
	}
	for _, m := range items {
		if m.IsDefault && m.Link.URL != "" {
			return m, true
		}
	}
	for _, m := range items {
		if m.Link.URL != "" {
			return m, true
		}
	}
	return media{}, false
}

func guessMime(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	}
	return "application/octet-stream"
}

func ParseBoolDefault(raw string, def bool) bool {
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return v
}

var _ drives.Drive = (*Driver)(nil)
