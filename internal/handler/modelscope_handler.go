package handler

import (
	"fmt"
	"strings"

	"dingospeed/internal/service"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
)

// ModelscopeHandler 模型代理请求处理器
type ModelscopeHandler struct {
	modelscopeService *service.ModelscopeService
}

// NewModelscopeHandler 创建模型代理处理器实例
func NewModelscopeHandler(modelscopeService *service.ModelscopeService) *ModelscopeHandler {
	return &ModelscopeHandler{
		modelscopeService: modelscopeService,
	}
}

// ModelInfoHandler 处理模型信息查询请求
func (m *ModelscopeHandler) ModelInfoHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	if len(parts) < 5 {
		err := fmt.Errorf("请求路径格式非法")
		return util.ResponseError(c, err)
	}

	org, repo, repoType := c.Param("org"), c.Param("repo"), c.Param("repoType")
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: repoType, Repo: org + "/" + repo}
	if err := key.Validate(); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	if err := repository.Register(config.SysConfig.Repos(), repository.Remote(key)); err != nil {
		return err
	}
	if err := m.modelscopeService.ForwardModelInfo(c, org, repo, repoType); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}

// RevisionsHandler 处理模型版本查询请求
func (m *ModelscopeHandler) RevisionsHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	if len(parts) < 5 {
		err := fmt.Errorf("请求路径格式非法")
		return util.ResponseError(c, err)
	}

	org, repo, repoType := c.Param("org"), c.Param("repo"), c.Param("repoType")
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: repoType, Repo: org + "/" + repo}
	if err := key.Validate(); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	if err := repository.Register(config.SysConfig.Repos(), repository.Remote(key)); err != nil {
		return err
	}
	if err := m.modelscopeService.ForwardRevisions(c, org, repo, repoType); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}

// FileListHandler 处理模型文件列表请求
func (m *ModelscopeHandler) FileListHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	if len(parts) < 5 {
		err := fmt.Errorf("请求路径格式非法")
		return util.ResponseError(c, err)
	}

	org, repo, repoType := c.Param("org"), c.Param("repo"), c.Param("repoType")
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: repoType, Repo: org + "/" + repo}
	if err := key.Validate(); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	if err := repository.Register(config.SysConfig.Repos(), repository.Remote(key)); err != nil {
		return err
	}
	if err := m.modelscopeService.ForwardFileList(c, org, repo, repoType); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}

// FileDownloadHandler 处理模型文件下载请求（支持续传）
func (m *ModelscopeHandler) FileDownloadHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	if len(parts) < 5 {
		err := fmt.Errorf("请求路径格式非法")
		return util.ResponseError(c, err)
	}

	org, repo, repoType := c.Param("org"), c.Param("repo"), c.Param("repoType")
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: repoType, Repo: org + "/" + repo}
	if err := key.Validate(); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	if err := repository.Register(config.SysConfig.Repos(), repository.Remote(key)); err != nil {
		return err
	}
	return m.modelscopeService.HandleFileDownload(c, org, repo, repoType)
}

// FileTreeHandler 处理数据集文件列表请求
func (m *ModelscopeHandler) FileTreeHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	if len(parts) < 5 {
		err := fmt.Errorf("请求路径格式非法")
		return util.ResponseError(c, err)
	}

	org, repo, repoType := c.Param("org"), c.Param("repo"), c.Param("repoType")
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: repoType, Repo: org + "/" + repo}
	if err := key.Validate(); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	if err := repository.Register(config.SysConfig.Repos(), repository.Remote(key)); err != nil {
		return err
	}
	if err := m.modelscopeService.ForwardRepoTree(c, org, repo, repoType); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}

// DatasetFileTreeHandler 处理数据集文件列表请求
func (m *ModelscopeHandler) DatasetFileTreeHandler(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")

	if len(parts) < 4 {
		err := fmt.Errorf("请求路径格式非法")
		return util.ResponseError(c, err)
	}

	datasetId := parts[3]
	if err := m.modelscopeService.ForwardRepoTreeByDatasetId(c, datasetId); err != nil {
		return util.ResponseError(c, err)
	}
	return nil
}
