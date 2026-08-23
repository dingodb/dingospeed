package handler

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/labstack/echo/v4"
)

const handlerBlockSize = 1024

// doChunk 走一次 PUT /api/local-upload-chunk/... 的完整 handler 路径。
func doChunk(t *testing.T, h *UploadHandler, e *echo.Echo, filePath string, content []byte,
	offset, length int64, chunkSha string) *httptest.ResponseRecorder {
	t.Helper()
	body := content[offset : offset+length]
	if chunkSha == "" {
		chunkSha = shaOf(body)
	}
	target := fmt.Sprintf("/api/local-upload-chunk/models/dingo-local/demo/main/%s?size=%d&sha256=%s&offset=%d&chunkSha256=%s",
		filePath, len(content), shaOf(content), offset, chunkSha)
	req := httptest.NewRequest(http.MethodPut, target, bytes.NewReader(body))
	// httptest.NewRequest 已按 body 长度填好 ContentLength，分块接口依赖它。
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("repoType", "org", "repo", "revision", "*")
	c.SetParamValues("models", "dingo-local", "demo", "main", filePath)
	if err := h.UploadChunk(c); err != nil {
		t.Fatalf("chunk handler returned error: %v", err)
	}
	return rec
}

func decodeChunkResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var payload map[string]interface{}
	if err := sonic.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not valid json: %v (%s)", err, rec.Body.String())
	}
	return payload
}

func TestChunkHandlerUploadsAndPublishes(t *testing.T) {
	h, e := newTestUploadHandler(t)
	content := bytes.Repeat([]byte("abcdefgh"), handlerBlockSize/8*3+1)

	// 两个 chunk：第一个是整块的整数倍，第二个是不足整块的尾巴。
	firstLen := int64(handlerBlockSize * 2)
	rec := doChunk(t, h, e, "weights.bin", content, 0, firstLen, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d (%s)", rec.Code, rec.Body.String())
	}
	payload := decodeChunkResponse(t, rec)
	if payload["status"] != "written" {
		t.Fatalf("unexpected first chunk payload: %v", payload)
	}
	if got := payload["blockSize"]; got != float64(handlerBlockSize) {
		t.Fatalf("response must carry the authoritative block size, got %v", got)
	}

	rec = doChunk(t, h, e, "weights.bin", content, firstLen, int64(len(content))-firstLen, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected tail chunk status: %d (%s)", rec.Code, rec.Body.String())
	}

	// 重复投递同一个 chunk 走幂等快路径。
	rec = doChunk(t, h, e, "weights.bin", content, 0, firstLen, "")
	payload = decodeChunkResponse(t, rec)
	if payload["status"] != "already_present" || payload["blocks"] != float64(0) {
		t.Fatalf("repeated chunk must be idempotent: %v", payload)
	}

	// 分块上传永远不生效，必须显式发布。
	body := fmt.Sprintf(`{"files":[{"path":"weights.bin","sha256":"%s","size":%d}]}`, shaOf(content), len(content))
	if rec = doPublish(t, h, e, body, ""); rec.Code != http.StatusCreated {
		t.Fatalf("publish after chunk upload failed: %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestChunkHandlerRejectsBadRequests(t *testing.T) {
	content := bytes.Repeat([]byte("z"), handlerBlockSize*2)

	t.Run("wrong chunk sha", func(t *testing.T) {
		h, e := newTestUploadHandler(t)
		rec := doChunk(t, h, e, "weights.bin", content, 0, handlerBlockSize, shaOf([]byte("something else")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unexpected status: %d (%s)", rec.Code, rec.Body.String())
		}
		if code := decodeChunkResponse(t, rec)["code"]; code != "UPLOAD_CHUNK_SHA_MISMATCH" {
			t.Fatalf("unexpected error code: %v", code)
		}
	})

	t.Run("misaligned offset", func(t *testing.T) {
		h, e := newTestUploadHandler(t)
		rec := doChunk(t, h, e, "weights.bin", content, 1, handlerBlockSize, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unexpected status: %d (%s)", rec.Code, rec.Body.String())
		}
		if code := decodeChunkResponse(t, rec)["code"]; code != "UPLOAD_INVALID_ARGUMENT" {
			t.Fatalf("unexpected error code: %v", code)
		}
	})
}

// 进度接口必须回出块大小与缺块区间，agent 据此切分与续传。
func TestChunkProgressReportsMissingRanges(t *testing.T) {
	h, e := newTestUploadHandler(t)
	content := bytes.Repeat([]byte("q"), handlerBlockSize*4)
	doChunk(t, h, e, "weights.bin", content, 0, handlerBlockSize, "")

	target := fmt.Sprintf("/api/local-upload-progress/models/dingo-local/demo/main/weights.bin?sha256=%s", shaOf(content))
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("repoType", "org", "repo", "revision", "*")
	c.SetParamValues("models", "dingo-local", "demo", "main", "weights.bin")
	if err := h.QueryProgress(c); err != nil {
		t.Fatalf("progress handler returned error: %v", err)
	}
	payload := decodeChunkResponse(t, rec)
	if payload["blockSize"] != float64(handlerBlockSize) {
		t.Fatalf("progress must report block size: %v", payload)
	}
	if payload["status"] != "uploading" || payload["blobComplete"] != false {
		t.Fatalf("unexpected progress payload: %v", payload)
	}
	ranges, ok := payload["missingRanges"].([]interface{})
	if !ok || len(ranges) != 1 {
		t.Fatalf("unexpected missing ranges: %v", payload["missingRanges"])
	}
	gap := ranges[0].(map[string]interface{})
	if gap["offset"] != float64(handlerBlockSize) || gap["length"] != float64(handlerBlockSize*3) {
		t.Fatalf("unexpected gap: %v", gap)
	}
}
