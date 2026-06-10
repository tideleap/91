package scriptcrawler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/video-site/backend/internal/catalog"
)

func TestCrawlerRunOnceImportsLocalFileAndSkipsExisting(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:     drv,
		Catalog:    cat,
		PythonPath: wrapper,
		ScriptPath: dummyScript,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("result = new:%d skipped:%d failed:%d, want 1/0/0", res.NewVideos, res.Skipped, res.Failed)
	}
	v, err := cat.GetVideo(ctx, BuildVideoID("demo", "abc-123"))
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if v.Title != "Imported From Helper" || v.FileID != "abc-123.mp4" || v.Size == 0 {
		t.Fatalf("video = title:%q file:%q size:%d", v.Title, v.FileID, v.Size)
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), "abc-123.mp4")); err != nil {
		t.Fatalf("video file not copied: %v", err)
	}

	res, err = c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.NewVideos != 0 || res.Skipped != 1 {
		t.Fatalf("second result = new:%d skipped:%d, want 0/1", res.NewVideos, res.Skipped)
	}
	if res.SeenSnapshot != 1 {
		t.Fatalf("seen snapshot = %d, want 1", res.SeenSnapshot)
	}
}

func TestCrawlerRunOnceUsesSourceKindNamespace(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:     drv,
		Catalog:    cat,
		SourceKind: "spider91",
		PythonPath: wrapper,
		ScriptPath: dummyScript,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.SeenSnapshot != 0 {
		t.Fatalf("result = new:%d seen:%d, want 1/0", res.NewVideos, res.SeenSnapshot)
	}
	videoID := BuildVideoIDForKind("spider91", "demo", "abc-123")
	if _, err := cat.GetVideo(ctx, videoID); err != nil {
		t.Fatalf("get source-kind video: %v", err)
	}
	if _, err := cat.GetVideo(ctx, BuildVideoID("demo", "abc-123")); err == nil {
		t.Fatalf("default namespace video unexpectedly exists")
	}

	res, err = c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.NewVideos != 0 || res.Skipped != 1 || res.SeenSnapshot != 1 {
		t.Fatalf("second result = new:%d skipped:%d seen:%d, want 0/1/1", res.NewVideos, res.Skipped, res.SeenSnapshot)
	}
}

func TestCrawlerRunOncePassesAbsoluteJobPathsWhenWorkDirDiffers(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	t.Chdir(tmp)
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	drv := New(Config{ID: "demo", RootDir: filepath.Join("data", "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	scriptDir := filepath.Join(tmp, "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}
	dummyScript := filepath.Join(scriptDir, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	t.Setenv("GO_WANT_SCRIPTCRAWLER_ASSERT_ABS", "1")
	c := NewCrawler(CrawlerConfig{
		Driver:     drv,
		Catalog:    cat,
		PythonPath: wrapper,
		ScriptPath: dummyScript,
		WorkDir:    scriptDir,
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("result = new:%d skipped:%d failed:%d, want 1/0/0", res.NewVideos, res.Skipped, res.Failed)
	}
	if !filepath.IsAbs(res.JobFile) || !filepath.IsAbs(res.SeenFile) {
		t.Fatalf("result paths should be absolute: job=%q seen=%q", res.JobFile, res.SeenFile)
	}
}

func TestCrawlerRunOnceImportsSimpleMediaURLWithoutSourceID(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/video.mp4" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("simple-video-bytes"))
	}))
	defer srv.Close()

	drv := New(Config{ID: "demo", RootDir: filepath.Join(tmp, "crawler")})
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("driver init: %v", err)
	}
	dummyScript := filepath.Join(tmp, "helper-script")
	if err := os.WriteFile(dummyScript, []byte("helper"), 0o755); err != nil {
		t.Fatalf("write dummy script: %v", err)
	}
	wrapper := filepath.Join(tmp, "helper-wrapper.sh")
	wrapperScript := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestScriptCrawlerHelperProcess \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	t.Setenv("GO_WANT_SCRIPTCRAWLER_HELPER", "1")
	t.Setenv("GO_WANT_SCRIPTCRAWLER_SIMPLE", "1")
	t.Setenv("GO_SCRIPTCRAWLER_MEDIA_URL", srv.URL+"/video.mp4?token=first")
	c := NewCrawler(CrawlerConfig{
		Driver:     drv,
		Catalog:    cat,
		PythonPath: wrapper,
		ScriptPath: dummyScript,
		HTTPClient: srv.Client(),
	})
	res, err := c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if res.NewVideos != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("result = new:%d skipped:%d failed:%d, want 1/0/0", res.NewVideos, res.Skipped, res.Failed)
	}
	videos, err := cat.ListVideosByDrive(ctx, "demo")
	if err != nil {
		t.Fatalf("list videos: %v", err)
	}
	if len(videos) != 1 {
		t.Fatalf("videos = %d, want 1", len(videos))
	}
	v := videos[0]
	if !strings.HasPrefix(v.ID, BuildVideoID("demo", "auto-")) {
		t.Fatalf("video id = %q, want generated auto source id", v.ID)
	}
	if v.Title != "Simple Protocol Video" || v.Ext != "mp4" || v.ThumbnailURL != "" || v.Size == 0 {
		t.Fatalf("video = title:%q ext:%q thumb:%q size:%d", v.Title, v.Ext, v.ThumbnailURL, v.Size)
	}
	if _, err := os.Stat(filepath.Join(drv.VideosDir(), v.FileID)); err != nil {
		t.Fatalf("video file not downloaded: %v", err)
	}

	t.Setenv("GO_SCRIPTCRAWLER_MEDIA_URL", srv.URL+"/video.mp4?token=second")
	res, err = c.RunOnce(ctx, 1)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.NewVideos != 0 || res.Skipped != 1 {
		t.Fatalf("second result = new:%d skipped:%d, want 0/1", res.NewVideos, res.Skipped)
	}
}

func TestScriptCrawlerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_HELPER") != "1" {
		return
	}
	args := os.Args
	jobPath := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--job" {
			jobPath = args[i+1]
			break
		}
	}
	if jobPath == "" {
		fmt.Fprintln(os.Stderr, "missing --job")
		os.Exit(2)
	}
	data, err := os.ReadFile(jobPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_ASSERT_ABS") == "1" {
		if !filepath.IsAbs(jobPath) || !filepath.IsAbs(job.SeenSourceIDsFile) || !filepath.IsAbs(job.OutputDir) {
			fmt.Fprintf(os.Stderr, "expected absolute paths, got job=%q seen=%q output=%q\n", jobPath, job.SeenSourceIDsFile, job.OutputDir)
			os.Exit(2)
		}
	}
	if os.Getenv("GO_WANT_SCRIPTCRAWLER_SIMPLE") == "1" {
		event := map[string]any{
			"title":     "Simple Protocol Video",
			"media_url": os.Getenv("GO_SCRIPTCRAWLER_MEDIA_URL"),
		}
		_ = json.NewEncoder(os.Stdout).Encode(event)
		os.Exit(0)
	}
	localFile := filepath.Join(job.OutputDir, "helper.mp4")
	if err := os.WriteFile(localFile, []byte("helper-video"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	event := Event{
		Type: "item",
		Item: Item{
			SourceID: "abc-123",
			Title:    "Imported From Helper",
			Author:   "helper",
			Media:    MediaRef{LocalFile: localFile},
		},
	}
	_ = json.NewEncoder(os.Stdout).Encode(event)
	os.Exit(0)
}
