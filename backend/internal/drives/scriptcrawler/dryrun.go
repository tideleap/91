package scriptcrawler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DryRun 在不入库的前提下试跑一个爬虫脚本：临时目录里生成 job.json，
// 启动脚本进程，拿到第一条（或前 MaxItems 条）item 事件后立即停止，
// 再对视频直链做一次小范围探测，验证脚本"能不能爬取到视频"。
// 用于后台导入脚本后的"测试脚本"按钮。

const (
	defaultDryRunTimeout  = 2 * time.Minute
	dryRunLogTailLines    = 60
	dryRunMediaProbeLimit = 20 * time.Second
)

type DryRunConfig struct {
	PythonPath string
	ScriptPath string
	ProxyURL   string
	ConfigJSON string
	// MaxItems 收到多少条 item 后停止脚本，默认 1。
	MaxItems int
	// Timeout 整个试跑的硬上限，默认 2 分钟。
	Timeout time.Duration
	// SkipMediaProbe 跳过视频直链可达性探测（单测注入用）。
	SkipMediaProbe bool
	HTTPClient     *http.Client
}

type DryRunItem struct {
	Title          string `json:"title"`
	SourceID       string `json:"sourceId,omitempty"`
	MediaURL       string `json:"mediaUrl,omitempty"`
	MediaLocalFile string `json:"mediaLocalFile,omitempty"`
	ThumbnailURL   string `json:"thumbnailUrl,omitempty"`
	DetailURL      string `json:"detailUrl,omitempty"`
}

type DryRunMediaCheck struct {
	OK            bool   `json:"ok"`
	Status        int    `json:"status,omitempty"`
	ContentType   string `json:"contentType,omitempty"`
	ContentLength int64  `json:"contentLengthBytes,omitempty"`
	Error         string `json:"error,omitempty"`
}

type DryRunResult struct {
	OK         bool              `json:"ok"`
	Items      []DryRunItem      `json:"items"`
	MediaCheck *DryRunMediaCheck `json:"mediaCheck,omitempty"`
	Error      string            `json:"error,omitempty"`
	Log        []string          `json:"log,omitempty"`
	DurationMs int64             `json:"durationMs"`
}

func DryRun(ctx context.Context, cfg DryRunConfig) *DryRunResult {
	started := time.Now()
	result := &DryRunResult{Items: []DryRunItem{}}
	defer func() { result.DurationMs = time.Since(started).Milliseconds() }()

	scriptPath := strings.TrimSpace(cfg.ScriptPath)
	if scriptPath == "" {
		result.Error = "脚本路径为空，请先导入脚本"
		return result
	}
	if _, err := os.Stat(scriptPath); err != nil {
		result.Error = fmt.Sprintf("脚本不存在: %v", err)
		return result
	}
	pythonPath := strings.TrimSpace(cfg.PythonPath)
	if pythonPath == "" {
		pythonPath = "python3"
	}
	maxItems := cfg.MaxItems
	if maxItems <= 0 {
		maxItems = 1
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultDryRunTimeout
	}

	tmpDir, err := os.MkdirTemp("", "crawler-dryrun-")
	if err != nil {
		result.Error = fmt.Sprintf("创建临时目录失败: %v", err)
		return result
	}
	defer os.RemoveAll(tmpDir)

	outputDir := filepath.Join(tmpDir, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		result.Error = fmt.Sprintf("创建输出目录失败: %v", err)
		return result
	}
	seenPath := filepath.Join(tmpDir, "seen.txt")
	if err := os.WriteFile(seenPath, nil, 0o644); err != nil {
		result.Error = fmt.Sprintf("写入 seen 文件失败: %v", err)
		return result
	}

	configJSON := json.RawMessage([]byte("{}"))
	if raw := strings.TrimSpace(cfg.ConfigJSON); raw != "" {
		if !json.Valid([]byte(raw)) {
			result.Error = "自定义配置必须是合法 JSON"
			return result
		}
		configJSON = json.RawMessage(raw)
	}
	job := Job{
		Protocol:          "crawler.v1",
		Mode:              "crawl",
		RunID:             "dryrun-" + started.UTC().Format("20060102T150405Z"),
		CrawlerID:         "dryrun",
		TargetNew:         maxItems,
		SeenSourceIDsFile: seenPath,
		OutputDir:         outputDir,
		Config:            configJSON,
		Network:           JobNetwork{ProxyURL: strings.TrimSpace(cfg.ProxyURL)},
	}
	jobPath := filepath.Join(tmpDir, "job.json")
	jobData, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		result.Error = fmt.Sprintf("生成 job 文件失败: %v", err)
		return result
	}
	if err := os.WriteFile(jobPath, jobData, 0o600); err != nil {
		result.Error = fmt.Sprintf("写入 job 文件失败: %v", err)
		return result
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, pythonPath, scriptPath, "--job", jobPath)
	cmd.Dir = filepath.Dir(scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return killDryRunProcess(cmd)
	}
	// 超时或提前 kill 后，脚本派生的子进程可能仍持有 stdout/stderr 管道；
	// WaitDelay 强制在宽限期后关闭管道，避免读取端永久阻塞。
	cmd.WaitDelay = 3 * time.Second
	if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" {
		cmd.Env = append(os.Environ(),
			"HTTP_PROXY="+proxyURL,
			"HTTPS_PROXY="+proxyURL,
			"http_proxy="+proxyURL,
			"https_proxy="+proxyURL,
			"NO_PROXY=",
			"no_proxy=",
		)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		result.Error = fmt.Sprintf("启动脚本失败: %v", err)
		return result
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		result.Error = fmt.Sprintf("启动脚本失败: %v", err)
		return result
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		result.Error = fmt.Sprintf("启动脚本失败: %v", err)
		return result
	}

	// stderr 是脚本日志，保留尾部若干行用于排错回显。
	var logMu sync.Mutex
	logTail := make([]string, 0, dryRunLogTailLines)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			logMu.Lock()
			if len(logTail) >= dryRunLogTailLines {
				logTail = logTail[1:]
			}
			logTail = append(logTail, line)
			logMu.Unlock()
		}
	}()

	items := []DryRunItem{}
	var firstMediaHeaders map[string]string
	parseFailures := 0
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if runCtx.Err() != nil {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			parseFailures++
			continue
		}
		eventType := strings.ToLower(strings.TrimSpace(event.Type))
		item := event.normalizedItem()
		if eventType == "" && item.hasPayload() {
			eventType = "item"
		}
		if eventType != "item" {
			continue
		}
		normalized, _, err := normalizeItemForImport(item)
		if err != nil {
			result.Error = fmt.Sprintf("item 字段不完整: %v", err)
			continue
		}
		mediaURL := strings.TrimSpace(normalized.Media.URL)
		if len(items) == 0 {
			firstMediaHeaders = normalized.Media.Headers
		}
		items = append(items, DryRunItem{
			Title:          strings.TrimSpace(normalized.Title),
			SourceID:       strings.TrimSpace(item.SourceID),
			MediaURL:       mediaURL,
			MediaLocalFile: strings.TrimSpace(normalized.Media.LocalFile),
			ThumbnailURL:   strings.TrimSpace(normalized.Thumbnail.URL),
			DetailURL:      strings.TrimSpace(normalized.DetailURL),
		})
		if len(items) >= maxItems {
			break
		}
	}
	// 拿够了就停掉脚本，避免它继续翻页。
	_ = killDryRunProcess(cmd)
	_ = cmd.Wait()
	<-stderrDone

	logMu.Lock()
	result.Log = append([]string{}, logTail...)
	logMu.Unlock()
	result.Items = items

	if len(items) == 0 {
		if result.Error == "" {
			switch {
			case runCtx.Err() != nil && ctx.Err() == nil:
				result.Error = fmt.Sprintf("测试超时（%s），脚本没有输出任何视频", timeout)
			case parseFailures > 0:
				result.Error = "脚本 stdout 不是合法的 crawler.v1 JSON Lines（日志应输出到 stderr）"
			default:
				result.Error = "脚本退出但没有输出任何视频"
			}
		}
		return result
	}
	result.Error = ""

	first := items[0]
	switch {
	case cfg.SkipMediaProbe:
		result.OK = true
	case first.MediaLocalFile != "":
		// 脚本自己下载到 output_dir 的模式：试跑用的是临时目录，
		// 文件已随目录清理，能输出合法 local_file 即视为通过。
		result.OK = true
	default:
		check := probeMediaURL(ctx, cfg, first, firstMediaHeaders)
		result.MediaCheck = check
		result.OK = check.OK
	}
	return result
}

func killDryRunProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return cmd.Process.Kill()
	}
	return nil
}

// probeMediaURL 对视频直链发一个 Range: bytes=0-0 的小请求，
// 验证直链可达（带上脚本给的防盗链 headers 和代理）。
func probeMediaURL(ctx context.Context, cfg DryRunConfig, item DryRunItem, mediaHeaders map[string]string) *DryRunMediaCheck {
	check := &DryRunMediaCheck{}
	if item.MediaURL == "" {
		check.Error = "item 没有视频直链"
		return check
	}

	client := cfg.HTTPClient
	if client == nil {
		transport := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: dryRunMediaProbeLimit,
		}
		if err := configureExplicitProxy(transport, cfg.ProxyURL); err != nil {
			check.Error = fmt.Sprintf("代理配置无效: %v", err)
			return check
		}
		client = &http.Client{Transport: transport}
	}

	probeCtx, cancel := context.WithTimeout(ctx, dryRunMediaProbeLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, item.MediaURL, nil)
	if err != nil {
		check.Error = fmt.Sprintf("视频直链无效: %v", err)
		return check
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Range", "bytes=0-0")
	if item.DetailURL != "" {
		req.Header.Set("Referer", item.DetailURL)
	}
	for k, v := range mediaHeaders {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		check.Error = fmt.Sprintf("视频直链请求失败: %v", err)
		return check
	}
	defer resp.Body.Close()

	check.Status = resp.StatusCode
	check.ContentType = resp.Header.Get("Content-Type")
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		// Content-Range: bytes 0-0/12345 → 取总大小
		if idx := strings.LastIndex(cr, "/"); idx >= 0 {
			var total int64
			if _, err := fmt.Sscanf(cr[idx+1:], "%d", &total); err == nil {
				check.ContentLength = total
			}
		}
	}
	if check.ContentLength == 0 && resp.StatusCode == http.StatusOK {
		check.ContentLength = resp.ContentLength
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		check.Error = fmt.Sprintf("视频直链返回 HTTP %d", resp.StatusCode)
		return check
	}
	check.OK = true
	return check
}
