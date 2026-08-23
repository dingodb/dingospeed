package handler

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"dingospeed/internal/dao"
	"dingospeed/internal/service"

	"github.com/bytedance/sonic"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type UploadHandler struct {
	uploadService *service.UploadService
}

func NewUploadHandler(uploadService *service.UploadService) *UploadHandler {
	return &UploadHandler{uploadService: uploadService}
}

func (h *UploadHandler) UploadWholeFile(c echo.Context) error {
	param := dao.LocalUploadParam{
		RepoType:  c.Param("repoType"),
		Org:       c.Param("org"),
		Repo:      c.Param("repo"),
		Revision:  c.Param("revision"),
		FilePath:  c.Param("*"),
		Sha256:    c.QueryParam("sha256"),
		Overwrite: strings.EqualFold(c.QueryParam("overwrite"), "true"),
		Deferred:  strings.EqualFold(c.QueryParam("defer"), "true"),
	}
	result, err := h.uploadService.UploadWholeFile(param, c.QueryParam("size"), c.QueryParam("start"), c.Request().Body)
	if err != nil {
		return writeUploadError(c, "local upload failed", err)
	}
	return c.JSON(http.StatusCreated, result)
}

// UploadChunk 接收整文件中的一个分块。
//
// 分块直接写进最终的 blobs/<sha>，没有 finalize 步骤：某个块位=1 表示“写入这块时它
// 通过了 chunk 级 sha 校验”，而“整个文件传完了”由 publish 时的位图检查判定。
// 分块上传因此永远是暂缓生效的，清单才是可见性闸门。
func (h *UploadHandler) UploadChunk(c echo.Context) error {
	param := dao.LocalChunkUploadParam{
		RepoType: c.Param("repoType"),
		Org:      c.Param("org"),
		Repo:     c.Param("repo"),
		Revision: c.Param("revision"),
		FilePath: c.Param("*"),
		Sha256:   c.QueryParam("sha256"),
	}
	result, err := h.uploadService.UploadChunk(
		param,
		c.QueryParam("size"),
		c.QueryParam("offset"),
		c.QueryParam("chunkSha256"),
		c.Request().ContentLength,
		c.Request().Body,
	)
	if err != nil {
		return writeUploadError(c, "local chunk upload failed", err)
	}
	return c.JSON(http.StatusOK, result)
}

// publishRequest 是批量发布的请求体。清单由调用方完整声明，服务端不记忆批次。
type publishRequest struct {
	Files []publishRequestFile `json:"files"`
}

type publishRequestFile struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// maxPublishBodyBytes 给发布请求体一个上限。清单条目本身很小（1000 条约 120KB），
// 这里只是防止一个畸形请求把内存吃光，与上传方向的无界写入防护是同一类保护。
const maxPublishBodyBytes = 8 << 20

func (h *UploadHandler) PublishFiles(c echo.Context) error {
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxPublishBodyBytes+1))
	if err != nil {
		return writeUploadError(c, "local publish failed", err)
	}
	if len(body) > maxPublishBodyBytes {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{
			"code":  "PUBLISH_BODY_TOO_LARGE",
			"error": fmt.Sprintf("publish body exceeds %d bytes", maxPublishBodyBytes),
		})
	}
	var req publishRequest
	if err = sonic.Unmarshal(body, &req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "PUBLISH_INVALID_ARGUMENT",
			"error": fmt.Sprintf("publish body is not valid json: %v", err),
		})
	}
	param := dao.LocalPublishParam{
		RepoType:  c.Param("repoType"),
		Org:       c.Param("org"),
		Repo:      c.Param("repo"),
		Revision:  c.Param("revision"),
		Overwrite: strings.EqualFold(c.QueryParam("overwrite"), "true"),
		Files:     make([]dao.LocalManifestFile, 0, len(req.Files)),
	}
	for _, item := range req.Files {
		param.Files = append(param.Files, dao.LocalManifestFile{
			Path:   item.Path,
			Sha256: item.Sha256,
			Size:   item.Size,
		})
	}
	result, err := h.uploadService.PublishFiles(param)
	if err != nil {
		return writeUploadError(c, "local publish failed", err)
	}
	return c.JSON(http.StatusCreated, result)
}

// publishTreeRequest 是整树发布的请求体。files 是目标全量清单，不是增量批次。
type publishTreeRequest struct {
	BaseCommit string               `json:"baseCommit"`
	Files      []publishRequestFile `json:"files"`
}

func (h *UploadHandler) PublishTree(c echo.Context) error {
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxPublishBodyBytes+1))
	if err != nil {
		return writeUploadError(c, "local publish tree failed", err)
	}
	if len(body) > maxPublishBodyBytes {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{
			"code":  "PUBLISH_BODY_TOO_LARGE",
			"error": fmt.Sprintf("publish body exceeds %d bytes", maxPublishBodyBytes),
		})
	}
	var req publishTreeRequest
	if err = sonic.Unmarshal(body, &req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "PUBLISH_TREE_INVALID_ARGUMENT",
			"error": fmt.Sprintf("publish body is not valid json: %v", err),
		})
	}
	param := dao.LocalPublishTreeParam{
		RepoType:   c.Param("repoType"),
		Org:        c.Param("org"),
		Repo:       c.Param("repo"),
		Revision:   c.Param("revision"),
		BaseCommit: req.BaseCommit,
		Files:      make([]dao.LocalManifestFile, 0, len(req.Files)),
	}
	for _, item := range req.Files {
		param.Files = append(param.Files, dao.LocalManifestFile{
			Path:   item.Path,
			Sha256: item.Sha256,
			Size:   item.Size,
		})
	}
	result, err := h.uploadService.PublishTree(param)
	if err != nil {
		return writeUploadError(c, "local publish tree failed", err)
	}
	return c.JSON(http.StatusCreated, result)
}

func (h *UploadHandler) QueryProgress(c echo.Context) error {
	param := dao.LocalUploadParam{
		RepoType: c.Param("repoType"),
		Org:      c.Param("org"),
		Repo:     c.Param("repo"),
		Revision: c.Param("revision"),
		FilePath: c.Param("*"),
		Sha256:   c.QueryParam("sha256"),
	}
	result, err := h.uploadService.QueryProgress(param)
	if err != nil {
		return writeUploadError(c, "local upload progress failed", err)
	}
	return c.JSON(http.StatusOK, result)
}

func writeUploadError(c echo.Context, logPrefix string, err error) error {
	zap.S().Warnf("%s: %v", logPrefix, err)
	if e, ok := err.(interface {
		StatusCode() int
		ErrorCode() string
	}); ok {
		return c.JSON(e.StatusCode(), map[string]string{"code": e.ErrorCode(), "error": err.Error()})
	}
	if e, ok := err.(interface{ StatusCode() int }); ok {
		return c.JSON(e.StatusCode(), map[string]string{"code": "LOCAL_UPLOAD_ERROR", "error": err.Error()})
	}
	return c.JSON(http.StatusInternalServerError, map[string]string{"code": "LOCAL_UPLOAD_INTERNAL_ERROR", "error": err.Error()})
}
