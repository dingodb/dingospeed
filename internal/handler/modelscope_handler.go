package handler

import (
	"strings"

	"dingospeed/internal/service"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
)

// ModelscopeHandler 模型代理请求处理器
type ModelscopeHandler struct {
	ModelscopeService *service.ModelscopeService
}

// NewModelscopeHandler 创建模型代理处理器实例
func NewModelscopeHandler(ModelscopeService *service.ModelscopeService) *ModelscopeHandler {
	return &ModelscopeHandler{
		ModelscopeService: ModelscopeService,
	}
}

// ModelInfoHandler 处理模型信息查询请求
func (h *ModelscopeHandler) ModelInfoHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	org, repo, repoType := parts[3], parts[4], parts[2]
	if err := h.ModelscopeService.ForwardModelInfo(c, org, repo, repoType); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}

// RevisionsHandler 处理模型版本查询请求
func (h *ModelscopeHandler) RevisionsHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	org, repo, repoType := parts[3], parts[4], parts[2]
	if err := h.ModelscopeService.ForwardRevisions(c, org, repo, repoType); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}

// FileListHandler 处理模型文件列表请求
func (h *ModelscopeHandler) FileListHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	org, repo, repoType := parts[3], parts[4], parts[2]
	if err := h.ModelscopeService.ForwardFileList(c, org, repo, repoType); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}

// FileDownloadHandler 处理模型文件下载请求（支持续传）
func (h *ModelscopeHandler) FileDownloadHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	org, repo, repoType := parts[3], parts[4], parts[2]
	if err := h.ModelscopeService.HandleFileDownload(c, org, repo, repoType); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}

// FileTreeHandler 处理数据集文件列表请求
func (h *ModelscopeHandler) FileTreeHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	org, repo, repoType := parts[3], parts[4], parts[2]
	if err := h.ModelscopeService.ForwardRepoTree(c, org, repo, repoType); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}

// DatasetFileTreeHandler 处理数据集文件列表请求
func (h *ModelscopeHandler) DatasetFileTreeHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	datasetId := parts[3]
	if err := h.ModelscopeService.ForwardRepoTreeByDatasetId(c, datasetId); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}
