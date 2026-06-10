package scriptcrawler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDryRunScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "crawler.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func TestDryRunCollectsFirstItem(t *testing.T) {
	script := writeDryRunScript(t, `
echo '[log] fetching list page' >&2
echo '{"type":"item","item":{"title":"Test Video","media_url":"https://cdn.example.test/v.mp4","source_id":"123","thumbnail_url":"https://cdn.example.test/t.jpg"}}'
echo '{"type":"done","stats":{"emitted":1}}'
`)
	result := DryRun(context.Background(), DryRunConfig{
		PythonPath:     "/bin/sh",
		ScriptPath:     script,
		SkipMediaProbe: true,
	})
	if !result.OK {
		t.Fatalf("ok = false, error = %q, log = %v", result.Error, result.Log)
	}
	if len(result.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(result.Items))
	}
	item := result.Items[0]
	if item.Title != "Test Video" || item.MediaURL != "https://cdn.example.test/v.mp4" || item.SourceID != "123" {
		t.Fatalf("item = %+v", item)
	}
	if len(result.Log) == 0 || !strings.Contains(result.Log[0], "fetching list page") {
		t.Fatalf("log tail = %v, want stderr captured", result.Log)
	}
}

func TestDryRunProbesMediaURL(t *testing.T) {
	var gotRange, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-0/4096")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)

	script := writeDryRunScript(t, fmt.Sprintf(
		`echo '{"type":"item","title":"Probe Video","media_url":"%s/v.mp4","detail_url":"https://example.test/view"}'`,
		srv.URL,
	))
	result := DryRun(context.Background(), DryRunConfig{
		PythonPath: "/bin/sh",
		ScriptPath: script,
	})
	if !result.OK {
		t.Fatalf("ok = false, error = %q, mediaCheck = %+v", result.Error, result.MediaCheck)
	}
	if result.MediaCheck == nil || !result.MediaCheck.OK {
		t.Fatalf("mediaCheck = %+v, want ok", result.MediaCheck)
	}
	if result.MediaCheck.Status != http.StatusPartialContent || result.MediaCheck.ContentLength != 4096 {
		t.Fatalf("mediaCheck = %+v, want 206 with total 4096", result.MediaCheck)
	}
	if gotRange != "bytes=0-0" || gotReferer != "https://example.test/view" {
		t.Fatalf("probe headers range=%q referer=%q", gotRange, gotReferer)
	}
}

func TestDryRunReportsBrokenMediaURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	script := writeDryRunScript(t, fmt.Sprintf(
		`echo '{"type":"item","title":"Dead Link","media_url":"%s/v.mp4"}'`,
		srv.URL,
	))
	result := DryRun(context.Background(), DryRunConfig{
		PythonPath: "/bin/sh",
		ScriptPath: script,
	})
	if result.OK {
		t.Fatal("ok = true, want false for HTTP 403 media url")
	}
	if result.MediaCheck == nil || result.MediaCheck.OK || result.MediaCheck.Status != http.StatusForbidden {
		t.Fatalf("mediaCheck = %+v, want failed 403", result.MediaCheck)
	}
	if len(result.Items) != 1 {
		t.Fatalf("items = %d, want item still returned for debugging", len(result.Items))
	}
}

func TestDryRunRejectsNonJSONStdout(t *testing.T) {
	script := writeDryRunScript(t, `echo 'plain text progress output'`)
	result := DryRun(context.Background(), DryRunConfig{
		PythonPath:     "/bin/sh",
		ScriptPath:     script,
		SkipMediaProbe: true,
	})
	if result.OK {
		t.Fatal("ok = true, want false for non-JSON stdout")
	}
	if !strings.Contains(result.Error, "JSON Lines") {
		t.Fatalf("error = %q, want JSON Lines hint", result.Error)
	}
}

func TestDryRunTimesOut(t *testing.T) {
	script := writeDryRunScript(t, `sleep 30`)
	start := time.Now()
	result := DryRun(context.Background(), DryRunConfig{
		PythonPath:     "/bin/sh",
		ScriptPath:     script,
		Timeout:        2 * time.Second,
		SkipMediaProbe: true,
	})
	if result.OK {
		t.Fatal("ok = true, want false on timeout")
	}
	if !strings.Contains(result.Error, "超时") {
		t.Fatalf("error = %q, want timeout message", result.Error)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("dry run took %s, script was not killed", elapsed)
	}
}

func TestDryRunMissingScript(t *testing.T) {
	result := DryRun(context.Background(), DryRunConfig{
		PythonPath: "/bin/sh",
		ScriptPath: filepath.Join(t.TempDir(), "missing.py"),
	})
	if result.OK || result.Error == "" {
		t.Fatalf("result = %+v, want error for missing script", result)
	}
}
