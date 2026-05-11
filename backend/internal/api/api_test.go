package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/catalog"
)

func TestVideoSourceUsesTranscodeForAvi(t *testing.T) {
	v := &catalog.Video{
		ID:      "video-1",
		DriveID: "drive-1",
		FileID:  "file-1",
		Ext:     "avi",
	}

	got := videoSource(v)

	if got != "/p/transcode/video-1" {
		t.Fatalf("video source = %q, want transcode route", got)
	}
}

func TestVideoSourceKeepsDirectStreamForMp4(t *testing.T) {
	v := &catalog.Video{
		ID:      "video-1",
		DriveID: "drive-1",
		FileID:  "file-1",
		Ext:     "mp4",
	}

	got := videoSource(v)

	if got != "/p/stream/drive-1/file-1" {
		t.Fatalf("video source = %q, want direct stream route", got)
	}
}

func TestTranscodeStatusReadyWhenCachedFileExists(t *testing.T) {
	s := &Server{LocalDir: t.TempDir()}
	videoID := "video-1"
	path := s.transcodePath(videoID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("mp4"), 0o644); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	if got := s.transcodeStatus(videoID); got != "ready" {
		t.Fatalf("status = %q, want ready", got)
	}
}

func TestTranscodeStatusProcessingWhenJobActive(t *testing.T) {
	s := &Server{LocalDir: t.TempDir()}
	videoID := "video-1"
	s.setTranscoding(videoID, true)

	if got := s.transcodeStatus(videoID); got != "processing" {
		t.Fatalf("status = %q, want processing", got)
	}
}

func TestTranscodeTempPathKeepsMp4Extension(t *testing.T) {
	s := &Server{LocalDir: t.TempDir()}

	if got := s.transcodeTempPath("video-1"); !strings.HasSuffix(got, ".mp4") {
		t.Fatalf("temp transcode path = %q, want .mp4 suffix for ffmpeg muxer detection", got)
	}
}

func TestHandleTagsReturnsUnifiedTagPool(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯女大后入",
		Tags:        []string{"后入", "女大"},
		Category:    "random-category",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleTags(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Count int    `json:"count"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	labels := make([]string, 0, len(got))
	for _, tag := range got {
		labels = append(labels, tag.Label)
	}
	if !containsString(labels, "清纯") {
		t.Fatalf("labels = %#v, want user tag 清纯", labels)
	}
	if !containsString(labels, "后入") {
		t.Fatalf("labels = %#v, want system tag 后入", labels)
	}
	var qingchunCount int
	for _, tag := range got {
		if tag.Label == "清纯" {
			qingchunCount = tag.Count
		}
	}
	if qingchunCount != 1 {
		t.Fatalf("清纯 count = %d, want 1; tags = %#v", qingchunCount, got)
	}
}

func TestHandleUpdateVideoTagsRejectsUnknownTags(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "普通标题",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	req := requestWithVideoID(http.MethodPut, "/api/video/video-1/tags", "video-1", strings.NewReader(`{"tags":["不存在"]}`))
	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleUpdateVideoTags(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleUpdateVideoTagsSavesExistingTags(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯标题",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}

	req := requestWithVideoID(http.MethodPut, "/api/video/video-1/tags", "video-1", strings.NewReader(`{"tags":["清纯"]}`))
	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleUpdateVideoTags(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"清纯"}) {
		t.Fatalf("tags = %#v, want 清纯", got.Tags)
	}
}

func TestHandleVideoDetailIncludesDriveKindLabel(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	now := time.Now()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:        "drive-onedrive",
		Kind:      "onedrive",
		Name:      "Personal Drive",
		RootID:    "root",
		Status:    "ok",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive-onedrive",
		FileID:      "file-1",
		Title:       "Video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	req := requestWithVideoID(http.MethodGet, "/api/video/video-1", "video-1", strings.NewReader(``))
	rr := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleVideoDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got VideoDetailDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SourceLabel != "OneDrive" {
		t.Fatalf("sourceLabel = %q, want OneDrive", got.SourceLabel)
	}
}

func TestHandleHideVideoRemovesVideoFromPublicListAndDetail(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	for _, v := range []*catalog.Video{
		{
			ID:          "video-hidden",
			DriveID:     "drive",
			FileID:      "file-hidden",
			Title:       "Hide me",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "video-visible",
			DriveID:     "drive",
			FileID:      "file-visible",
			Title:       "Keep me",
			PublishedAt: now.Add(-time.Minute),
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	server := &Server{Catalog: cat}
	hideReq := requestWithVideoID(http.MethodPost, "/api/video/video-hidden/hide", "video-hidden", strings.NewReader(``))
	hideRR := httptest.NewRecorder()
	server.handleHideVideo(hideRR, hideReq)

	if hideRR.Code != http.StatusOK {
		t.Fatalf("hide status = %d, body = %s", hideRR.Code, hideRR.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/list?page=1&size=24", nil)
	listRR := httptest.NewRecorder()
	server.handleList(listRR, listReq)

	if listRR.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRR.Code, listRR.Body.String())
	}
	var listed struct {
		Items []VideoDTO `json:"items"`
		Total int        `json:"total"`
	}
	if err := json.NewDecoder(listRR.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].ID != "video-visible" {
		t.Fatalf("listed = total:%d items:%#v, want only video-visible", listed.Total, listed.Items)
	}

	detailReq := requestWithVideoID(http.MethodGet, "/api/video/video-hidden", "video-hidden", strings.NewReader(``))
	detailRR := httptest.NewRecorder()
	server.handleVideoDetail(detailRR, detailReq)

	if detailRR.Code != http.StatusNotFound {
		t.Fatalf("detail status = %d, want 404; body = %s", detailRR.Code, detailRR.Body.String())
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func requestWithVideoID(method, target, videoID string, body *strings.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", videoID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	return req
}
