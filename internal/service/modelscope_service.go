package service

import (
	"bytes"
	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"encoding/json"
	"fmt"
	"github.com/labstack/echo/v4"
	"github.com/patrickmn/go-cache"
	"go.uber.org/zap"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ModelscopeService struct{ fileDao *dao.FileDao }

func NewModelscopeService() *ModelscopeService {
	// The canonical handler is also built before config loading; do not initialize global data here.
	base := &data.BaseData{Cache: cache.New(24*time.Hour, time.Minute)}
	return &ModelscopeService{fileDao: dao.NewFileDao(dao.NewDownloaderDao(nil), base, dao.NewLockDao(base))}
}
func (m *ModelscopeService) ForwardModelInfo(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)
	zap.S().Infof("转发%s信息请求到官方: %s", apiPrefix, officialURL)
	return m.forwardRequest(c, officialURL)
}

func (m *ModelscopeService) ForwardRevisions(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/revisions?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)
	zap.S().Infof("转发%s版本请求到官方: %s", apiPrefix, officialURL)
	return m.forwardRequest(c, officialURL)
}

func (m *ModelscopeService) ForwardFileList(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/repo/files?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)
	zap.S().Infof("转发%s文件列表请求到官方: %s", apiPrefix, officialURL)
	return m.forwardRequest(c, officialURL)
}

func (m *ModelscopeService) ForwardRepoTree(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/repo/tree?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)
	zap.S().Infof("转发%s文件树请求到官方: %s", apiPrefix, officialURL)
	return m.forwardRequest(c, officialURL)
}

func (m *ModelscopeService) ForwardRepoTreeByDatasetId(c echo.Context, datasetId string) error {
	officialURL := fmt.Sprintf("%s/api/v1/datasets/%s/repo/tree?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		url.PathEscape(datasetId),
		c.Request().URL.RawQuery)
	zap.S().Infof("转发文件树请求到官方: %s", officialURL)
	return m.forwardRequest(c, officialURL)
}

// forwardRequest 通用请求转发逻辑
func (m *ModelscopeService) forwardRequest(c echo.Context, officialURL string) error {
	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodGet, officialURL, nil)
	if err != nil {
		zap.S().Errorf("构建请求失败: %v", err)
		return err
	}

	util.AddCLIHeaders(req.Header, c.Request().Header.Get("User-Agent"))

	for k, v := range c.Request().Header {
		if strings.EqualFold(k, "X-Dingo-Service-Token") {
			continue
		}
		req.Header[k] = v
	}

	resp, err := util.DoRequestWithRetry(req)
	if err != nil {
		zap.S().Errorf("转发请求失败: %v", err)
		return err
	}
	defer resp.Body.Close()

	var body io.Reader = resp.Body
	if resp.StatusCode == 200 && strings.HasSuffix(c.Request().URL.Path, "/repo/files") && strings.EqualFold(c.QueryParam("Recursive"), "true") && c.QueryParam("Root") == "" {
		raw, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return readErr
		}
		var envelope modelscopeEnvelope
		if json.Unmarshal(raw, &envelope) == nil && (envelope.Code == 0 || envelope.Code == 200) && (envelope.Data.LatestCommitter.Id != "" || envelope.Data.LatestCommitter.ShortId != "") {
			revision := c.QueryParam("Revision")
			if revision == "" {
				revision = "master"
			}
			typ := "models"
			if strings.Contains(c.Request().URL.Path, "/datasets/") {
				typ = "datasets"
			}
			key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: typ, Repo: c.Param("org") + "/" + c.Param("repo")}
			if err = repository.Register(config.SysConfig.Repos(), repository.Remote(key)); err != nil {
				return err
			}
			if _, err = m.cacheRepositoryMetadata(key, revision, &envelope); err != nil {
				return err
			}
		}
		body = bytes.NewReader(raw)
	}
	for k, v := range resp.Header {
		if k == "Link" || k == "Location" {
			for i := range v {
				v[i] = strings.ReplaceAll(v[i], config.SysConfig.Modelscope.OfficialBaseURL, providerLinkBase(c))
			}
		}
		c.Response().Header()[k] = v
	}
	c.Response().WriteHeader(resp.StatusCode)

	_, err = io.Copy(c.Response(), body)
	if err != nil {
		zap.S().Errorf("复制响应体失败: %v", err)
		return err
	}
	return nil
}
