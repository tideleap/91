package pikpak

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/go-resty/resty/v2"
)

// rewritingTransport 把 *.mypikpak.net 的请求劫持到 httptest server。
// PikPak API URL 在 driver 里硬编码成 const，测试不动产线代码，
// 通过 transport 改写实现。
type rewritingTransport struct {
	base   http.RoundTripper
	target string // httptest server 的 host（含端口）
}

func (rt *rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Host, "mypikpak.net") {
		req.URL.Scheme = "http"
		req.URL.Host = rt.target
	}
	return rt.base.RoundTrip(req)
}

// newTestDriver 拼一个最小可用的 Driver：跳过登录，把 client 的 transport
// 重定向到测试服务器，access token 也注入一个固定值避免触发 401 重试链。
func newTestDriver(t *testing.T, server *httptest.Server) *Driver {
	t.Helper()
	d := New(Config{
		ID:           "pikpak-test",
		Username:     "test@example.com",
		Password:     "ignored",
		Platform:     "web",
		RootID:       "root-id",
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		CaptchaToken: "test-captcha",
		DeviceID:     "test-device",
	})
	d.client = resty.New().SetHeader("Accept", "application/json")
	d.client.SetTransport(&rewritingTransport{
		base:   http.DefaultTransport,
		target: server.Listener.Addr().String(),
	})
	return d
}

// uploadRequestBody 用来反序列化 POST /drive/v1/files 的请求体。
type uploadRequestBody struct {
	Kind        string         `json:"kind"`
	Name        string         `json:"name"`
	Size        int64          `json:"size"`
	Hash        string         `json:"hash"`
	UploadType  string         `json:"upload_type"`
	ObjProvider map[string]any `json:"objProvider"`
	ParentID    string         `json:"parent_id"`
	FolderType  string         `json:"folder_type"`
}

func TestUploadInstantSuccessReturnsFileID(t *testing.T) {
	var got uploadRequestBody
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Query().Get("usage") != "" {
			t.Errorf("upload request must not carry usage query: %q", r.URL.RawQuery)
		}
		if h := r.Header.Get("Authorization"); h != "Bearer test-access-token" {
			t.Errorf("Authorization = %q", h)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		// resumable=null 表示秒传命中
		_, _ = w.Write([]byte(`{
			"upload_type": "UPLOAD_TYPE_RESUMABLE",
			"resumable":   null,
			"file":        {"id": "instant-file-id", "name": "test.mp4", "kind": "drive#file"}
		}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)

	body := bytes.Repeat([]byte{0xAB}, 4096)
	id, err := d.Upload(context.Background(), "parent-id", "test.mp4", bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if id != "instant-file-id" {
		t.Fatalf("file id = %q, want instant-file-id", id)
	}

	// 验证发出去的请求体 schema 跟 PikPak API 文档一致
	if got.Kind != "drive#file" {
		t.Errorf("kind = %q, want drive#file", got.Kind)
	}
	if got.Name != "test.mp4" {
		t.Errorf("name = %q, want test.mp4", got.Name)
	}
	if got.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", got.Size, len(body))
	}
	if got.UploadType != "UPLOAD_TYPE_RESUMABLE" {
		t.Errorf("upload_type = %q, want UPLOAD_TYPE_RESUMABLE", got.UploadType)
	}
	if got.ParentID != "parent-id" {
		t.Errorf("parent_id = %q, want parent-id", got.ParentID)
	}
	if got.FolderType != "NORMAL" {
		t.Errorf("folder_type = %q, want NORMAL", got.FolderType)
	}
	if got.ObjProvider["provider"] != "UPLOAD_TYPE_UNKNOWN" {
		t.Errorf("objProvider.provider = %v, want UPLOAD_TYPE_UNKNOWN", got.ObjProvider["provider"])
	}
	if len(got.Hash) != 40 {
		t.Errorf("hash length = %d, want 40 (GCID hex)", len(got.Hash))
	}
	if got.Hash != strings.ToUpper(got.Hash) {
		t.Errorf("hash should be uppercase: %q", got.Hash)
	}
	// 校验 hash 实际值就是 body 的 GCID
	wantHash := computeExpectedGCID(body)
	if got.Hash != wantHash {
		t.Errorf("hash = %s, want %s", got.Hash, wantHash)
	}
}

func TestUploadInstantSuccessFallsBackToListWhenFileIDMissing(t *testing.T) {
	listCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			// 秒传命中但 file.id 为空（极少数情况）
			_, _ = w.Write([]byte(`{"upload_type": "UPLOAD_TYPE_RESUMABLE", "resumable": null, "file": {}}`))
		case http.MethodGet:
			listCalled = true
			if got := r.URL.Query().Get("parent_id"); got != "parent-id" {
				t.Errorf("list parent_id = %q, want parent-id", got)
			}
			_, _ = w.Write([]byte(`{
				"files": [
					{"id": "other", "name": "other.mp4", "kind": "drive#file"},
					{"id": "found-via-list", "name": "test.mp4", "kind": "drive#file"}
				],
				"next_page_token": ""
			}`))
		default:
			t.Errorf("unexpected method %q", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	body := bytes.NewReader([]byte("payload"))
	id, err := d.Upload(context.Background(), "parent-id", "test.mp4", body, 7)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if id != "found-via-list" {
		t.Fatalf("file id = %q, want found-via-list", id)
	}
	if !listCalled {
		t.Fatal("expected fallback list call when file.id is empty")
	}
}

func TestUploadRetriesWithNewSessionWhenOSSEndpointDNSFails(t *testing.T) {
	sessionRequests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		sessionRequests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{
			"upload_type": "UPLOAD_TYPE_RESUMABLE",
			"resumable": {
				"kind": "drive#resumable",
				"provider": "UPLOAD_TYPE_UNKNOWN",
				"params": {
					"access_key_id": "ak",
					"access_key_secret": "sk",
					"bucket": "bucket",
					"endpoint": "https://vip-lixian-%02d.upload-a10b.mypikpak.com",
					"key": "object-key-%02d",
					"security_token": "token"
				}
			},
			"file": {"id": "retry-file-%02d", "name": "retry.mp4", "kind": "drive#file"}
		}`, sessionRequests, sessionRequests, sessionRequests)))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)
	uploadAttempts := 0
	var uploaded []byte
	d.uploadToOSSFunc = func(_ context.Context, _ *s3Params, body io.Reader) error {
		uploadAttempts++
		if uploadAttempts == 1 {
			return &net.DNSError{Err: "no such host", Name: "vip-lixian-01.upload-a10b.mypikpak.com"}
		}
		var err error
		uploaded, err = io.ReadAll(body)
		return err
	}

	payload := []byte("retry payload body")
	id, err := d.Upload(context.Background(), "parent-id", "retry.mp4", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if id != "retry-file-02" {
		t.Fatalf("file id = %q, want retry-file-02 from the second session", id)
	}
	if sessionRequests != 2 {
		t.Fatalf("session requests = %d, want 2", sessionRequests)
	}
	if uploadAttempts != 2 {
		t.Fatalf("upload attempts = %d, want 2", uploadAttempts)
	}
	if !bytes.Equal(uploaded, payload) {
		t.Fatalf("uploaded body = %q, want %q", string(uploaded), string(payload))
	}
}

func TestPikPakOSSClientUsesCNAMEForPikPakUploadEndpoint(t *testing.T) {
	params := &s3Params{
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
		Bucket:          "vip-lixian-07",
		Endpoint:        "http://upload-a10b.mypikpak.com",
		Key:             "upload_tmp/object-key",
	}
	client, err := newPikPakOSSClient(params)
	if err != nil {
		t.Fatalf("new oss client: %v", err)
	}
	bucket, err := client.Bucket(params.Bucket)
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	signed, err := bucket.SignURL(params.Key, oss.HTTPPut, 60)
	if err != nil {
		t.Fatalf("sign url: %v", err)
	}
	if strings.Contains(signed, "vip-lixian-07.upload-a10b.mypikpak.com") {
		t.Fatalf("signed url uses invalid bucket-prefixed PikPak host: %s", signed)
	}
	if !strings.Contains(signed, "http://upload-a10b.mypikpak.com/upload_tmp%2Fobject-key") {
		t.Fatalf("signed url = %s, want PikPak endpoint host with object key path", signed)
	}
}

func TestUploadRejectsInvalidArguments(t *testing.T) {
	d := New(Config{ID: "x", Username: "u", Password: "p", Platform: "web"})
	cases := []struct {
		name    string
		parent  string
		fname   string
		size    int64
		reader  io.Reader
		wantErr string
	}{
		{"nil reader", "parent", "f.mp4", 0, nil, "nil reader"},
		{"negative size", "parent", "f.mp4", -1, bytes.NewReader(nil), "invalid size"},
		{"empty name", "parent", "   ", 1, bytes.NewReader([]byte{0}), "empty file name"},
		{"oversized", "parent", "f.mp4", 6 * 1024 * 1024 * 1024, bytes.NewReader([]byte{0}), "exceeds"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := d.Upload(context.Background(), c.parent, c.fname, c.reader, c.size)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestBufferAndHashGCIDDetectsSizeMismatch(t *testing.T) {
	src := bytes.NewReader([]byte("hello"))
	// 声明 size=10 但实际只有 5 字节
	_, _, _, err := bufferAndHashGCID(src, 10)
	if err == nil {
		t.Fatal("expected size mismatch error")
	}
	if !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("error = %v, want size mismatch", err)
	}
}

func TestBufferAndHashGCIDComputesCorrectHash(t *testing.T) {
	data := bytes.Repeat([]byte{0x55}, 1024)
	tmp, hex, written, err := bufferAndHashGCID(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("buffer: %v", err)
	}
	defer tmp.Close()

	if written != int64(len(data)) {
		t.Fatalf("written = %d, want %d", written, len(data))
	}
	want := computeExpectedGCID(data)
	if hex != want {
		t.Fatalf("hash = %s, want %s", hex, want)
	}
}

// computeExpectedGCID 给测试用的简易 GCID 计算（只对小数据足够）。
func computeExpectedGCID(data []byte) string {
	var blockSize int64 = 0x40000
	size := int64(len(data))
	for size > 0 && float64(size)/float64(blockSize) > 0x200 && blockSize < 0x200000 {
		blockSize <<= 1
	}
	outer := sha1.New()
	for off := int64(0); off < size; off += blockSize {
		end := off + blockSize
		if end > size {
			end = size
		}
		inner := sha1.New()
		inner.Write(data[off:end])
		outer.Write(inner.Sum(nil))
	}
	return strings.ToUpper(hex.EncodeToString(outer.Sum(nil)))
}

func TestRenameSendsPatchWithName(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   map[string]any
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	d := newTestDriver(t, server)

	if err := d.Rename(context.Background(), "file-id-xyz", "new name.mp4"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/drive/v1/files/file-id-xyz" {
		t.Errorf("path = %q, want /drive/v1/files/file-id-xyz", gotPath)
	}
	if gotBody["name"] != "new name.mp4" {
		t.Errorf("body name = %v, want \"new name.mp4\"", gotBody["name"])
	}
}

func TestRenameRejectsEmptyArguments(t *testing.T) {
	d := New(Config{ID: "x", Username: "u", Password: "p"})
	if err := d.Rename(context.Background(), "", "name"); err == nil {
		t.Fatal("expected error on empty file id")
	}
	if err := d.Rename(context.Background(), "file-id", ""); err == nil {
		t.Fatal("expected error on empty new name")
	}
	if err := d.Rename(context.Background(), "file-id", "   "); err == nil {
		t.Fatal("expected error on whitespace-only new name")
	}
}
