package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/service"
	"dingospeed/pkg/config"

	"github.com/bytedance/sonic"
	"github.com/labstack/echo/v4"
)

func newTestUploadHandler(t *testing.T) (*UploadHandler, *echo.Echo) {
	t.Helper()
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{
		Server:   config.ServerConfig{Repos: t.TempDir()},
		Download: config.Download{BlockSize: 1024},
		Upload:   config.Upload{Namespace: "dingo-local", Token: "secret", ConcurrentLimit: 4},
	}
	t.Cleanup(func() { config.SysConfig = oldConfig })

	baseData := data.NewBaseData()
	lockDao := dao.NewLockDao(baseData)
	uploadDao := dao.NewUploadDao(dao.NewFileDao(nil, baseData, lockDao), lockDao)
	return NewUploadHandler(service.NewUploadService(uploadDao)), echo.New()
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// doUpload 走一次 POST /api/local-upload/... 的完整 handler 路径。
func doUpload(t *testing.T, h *UploadHandler, e *echo.Echo, filePath string, content []byte, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := fmt.Sprintf("/api/local-upload/models/dingo-local/demo/main/%s?size=%d&sha256=%s&%s",
		filePath, len(content), shaOf(content), query)
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(content))
	req.Header.Set(uploadTokenHeader, "secret")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("repoType", "org", "repo", "revision", "*")
	c.SetParamValues("models", "dingo-local", "demo", "main", filePath)
	if err := h.UploadWholeFile(c); err != nil {
		t.Fatalf("upload handler returned error: %v", err)
	}
	return rec
}

func doPublish(t *testing.T, h *UploadHandler, e *echo.Echo, body string, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/local-publish/models/dingo-local/demo/main?" + query
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set(uploadTokenHeader, "secret")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("repoType", "org", "repo", "revision")
	c.SetParamValues("models", "dingo-local", "demo", "main")
	if err := h.PublishFiles(c); err != nil {
		t.Fatalf("publish handler returned error: %v", err)
	}
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := sonic.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not valid json (%s): %v", rec.Body.String(), err)
	}
	return out
}

// §4.2 调用形态：N 条上传 + 1 条发布走完整的 HTTP 路径。
func TestUploadDeferredThenPublishOverHTTP(t *testing.T) {
	h, e := newTestUploadHandler(t)

	files := map[string][]byte{
		"config.json":           []byte(`{"model_type":"demo"}`),
		"subdir/tokenizer.json": []byte(`{"version":"1.0"}`),
	}
	entries := make([]map[string]interface{}, 0, len(files))
	for name, content := range files {
		rec := doUpload(t, h, e, name, content, "defer=true")
		if rec.Code != http.StatusCreated {
			t.Fatalf("deferred upload of %s returned %d: %s", name, rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)
		if body["status"] != "staged" {
			t.Fatalf("deferred upload status is %v, want staged", body["status"])
		}
		if body["commit"] != "" {
			t.Fatalf("deferred upload must not return a commit, got %v", body["commit"])
		}
		entries = append(entries, map[string]interface{}{
			"path": name, "sha256": shaOf(content), "size": len(content),
		})
	}

	payload, err := sonic.Marshal(map[string]interface{}{"files": entries})
	if err != nil {
		t.Fatalf("marshal publish body failed: %v", err)
	}
	rec := doPublish(t, h, e, string(payload), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish returned %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["status"] != "published" || body["changed"] != true {
		t.Fatalf("unexpected publish response: %s", rec.Body.String())
	}
	if commit, _ := body["commit"].(string); len(commit) != 64 {
		t.Fatalf("publish did not return a commit: %s", rec.Body.String())
	}
	if count, _ := body["fileCount"].(float64); int(count) != len(files) {
		t.Fatalf("publish fileCount is %v, want %d", body["fileCount"], len(files))
	}
}

// 不带 defer 参数时，单文件上传的行为必须与增量前完全一致：传完即生效。
func TestUploadWithoutDeferStillPublishesImmediately(t *testing.T) {
	h, e := newTestUploadHandler(t)

	content := []byte("immediate content")
	rec := doUpload(t, h, e, "config.json", content, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload returned %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["status"] != "effective" {
		t.Fatalf("upload status is %v, want effective", body["status"])
	}
	if commit, _ := body["commit"].(string); len(commit) != 64 {
		t.Fatalf("upload did not return a commit: %s", rec.Body.String())
	}
}

func TestPublishRejectsMalformedBody(t *testing.T) {
	h, e := newTestUploadHandler(t)

	t.Run("invalid json", func(t *testing.T) {
		rec := doPublish(t, h, e, "{not json", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid json returned %d: %s", rec.Code, rec.Body.String())
		}
		if decodeBody(t, rec)["code"] != "PUBLISH_INVALID_ARGUMENT" {
			t.Fatalf("unexpected error code: %s", rec.Body.String())
		}
	})

	t.Run("empty file list", func(t *testing.T) {
		rec := doPublish(t, h, e, `{"files":[]}`, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("empty list returned %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("body too large", func(t *testing.T) {
		rec := doPublish(t, h, e, strings.Repeat("x", maxPublishBodyBytes+1), "")
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized body returned %d", rec.Code)
		}
		if decodeBody(t, rec)["code"] != "PUBLISH_BODY_TOO_LARGE" {
			t.Fatalf("unexpected error code: %s", rec.Body.String())
		}
	})
}

// §9.4：发布清单里有内容没传完时整次拒绝，并且响应里要能看出是哪个路径。
func TestPublishOverHTTPReportsUnreadyPaths(t *testing.T) {
	h, e := newTestUploadHandler(t)

	ready := []byte("ready content")
	if rec := doUpload(t, h, e, "ready.bin", ready, "defer=true"); rec.Code != http.StatusCreated {
		t.Fatalf("deferred upload failed: %s", rec.Body.String())
	}
	missing := []byte("never uploaded")
	payload := fmt.Sprintf(`{"files":[{"path":"ready.bin","sha256":%q,"size":%d},{"path":"ghost.bin","sha256":%q,"size":%d}]}`,
		shaOf(ready), len(ready), shaOf(missing), len(missing))

	rec := doPublish(t, h, e, payload, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish with unready content returned %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["code"] != "PUBLISH_CONTENT_NOT_READY" {
		t.Fatalf("unexpected error code: %s", rec.Body.String())
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "ghost.bin") {
		t.Fatalf("error must name the unready path: %s", rec.Body.String())
	}
}

func TestPublishRequiresToken(t *testing.T) {
	h, e := newTestUploadHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/api/local-publish/models/dingo-local/demo/main",
		strings.NewReader(`{"files":[{"path":"a.bin","sha256":"`+strings.Repeat("a", 64)+`","size":1}]}`))
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("repoType", "org", "repo", "revision")
	c.SetParamValues("models", "dingo-local", "demo", "main")
	if err := h.PublishFiles(c); err != nil {
		t.Fatalf("publish handler returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("publish without a token returned %d: %s", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["code"] != "UPLOAD_TOKEN_MISSING" {
		t.Fatalf("unexpected error code: %s", rec.Body.String())
	}
}
