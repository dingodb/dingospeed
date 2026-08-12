package handler

import (
	"net/http"
	"strings"

	"dingospeed/internal/dao"
	"dingospeed/internal/service"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

const uploadTokenHeader = "X-Dingo-Upload-Token"

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
	}
	result, err := h.uploadService.UploadWholeFile(param, c.QueryParam("size"), c.QueryParam("start"), c.Request().Header.Get(uploadTokenHeader), c.Request().Body)
	if err != nil {
		return writeUploadError(c, "local upload failed", err)
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
	result, err := h.uploadService.QueryProgress(param, c.Request().Header.Get(uploadTokenHeader))
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
