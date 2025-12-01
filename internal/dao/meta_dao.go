//  Copyright (c) 2025 dingodb.com, Inc. All Rights Reserved
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http:www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package dao

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"dingospeed/internal/data"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type MetaDao struct {
	fileDao  *FileDao
	lockDao  *LockDao
	baseData *data.BaseData
}

func NewMetaDao(fileDao *FileDao, lockDao *LockDao, baseData *data.BaseData) *MetaDao {
	return &MetaDao{
		fileDao:  fileDao,
		lockDao:  lockDao,
		baseData: baseData,
	}
}

func (m *MetaDao) WhoamiV2Generator(c echo.Context) error {
	newHeaders := make(map[string]string, 0)
	for k := range c.Request().Header {
		v := c.Request().Header.Get(k)
		lowerKey := strings.ToLower(k)
		if lowerKey == "host" {
			continue
		}
		newHeaders[lowerKey] = v
	}
	resp, err := util.Get("/api/whoami-v2", newHeaders)
	if err != nil {
		zap.S().Errorf("WhoamiV2Generator err.%v", err)
		return err
	}
	extractHeaders := resp.ExtractHeaders(resp.Headers)
	for k, vv := range extractHeaders {
		c.Response().Header().Add(k, vv)
	}
	c.Response().WriteHeader(resp.StatusCode)
	if _, err := c.Response().Write(resp.Body); err != nil {
		zap.S().Errorf("响应内容回传失败.%v", err)
	}
	return nil
}

func (m *MetaDao) ReposGenerator(c echo.Context) error {
	reposPath := config.SysConfig.Repos()

	datasets, _ := filepath.Glob(filepath.Join(reposPath, "api/datasets/*/*"))
	datasetsRepos := util.ProcessPaths(datasets)

	models, _ := filepath.Glob(filepath.Join(reposPath, "api/models/*/*"))
	modelsRepos := util.ProcessPaths(models)

	spaces, _ := filepath.Glob(filepath.Join(reposPath, "api/spaces/*/*"))
	spacesRepos := util.ProcessPaths(spaces)

	return c.Render(http.StatusOK, "repos.html", map[string]interface{}{
		"datasets_repos": datasetsRepos,
		"models_repos":   modelsRepos,
		"spaces_repos":   spacesRepos,
	})
}

func (m *MetaDao) RepoRefs(repoType string, orgRepo string, authorization string) (*common.Response, error) {
	refsUri := fmt.Sprintf("/api/%s/%s/refs", repoType, orgRepo)
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	resp, err := util.RetryRequest(func() (*common.Response, error) {
		return util.Get(refsUri, headers)
	})
	return resp, err
}

func (m *MetaDao) ForwardRefs(originalReq echo.Context) (*http.Response, error) {
	return util.ForwardRequest(originalReq)
}

func (m *MetaDao) GetMetadata(repoType, orgRepo, commit, method, authorization string) (*common.CacheContent, error) {
	var (
		cacheContent *common.CacheContent
		err          error
	)
	orgRepoKey := GetMetaDataReqKey(repoType, orgRepo, commit)
	lock := m.lockDao.getMetaDataReqLock(orgRepoKey)
	lock.Lock()
	defer lock.Unlock()
	commitSha, err := m.fileDao.GetFileCommitSha(repoType, orgRepo, commit, authorization)
	if err != nil {
		return nil, err
	}
	apiDir := fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.Repos(), repoType, orgRepo, commitSha)
	apiMetaPath := fmt.Sprintf("%s/%s", apiDir, fmt.Sprintf("meta_%s.json", method))
	if config.SysConfig.Online() {
		if util.FileExists(apiMetaPath) {
			if cacheContent, err = m.fileDao.ReadCacheRequest(apiMetaPath); err != nil {
				zap.S().Errorf("ReadCacheRequest err.%v", err)
				if cacheContent, err = m.requestAndSaveMeta(repoType, orgRepo, commit, commitSha, method, authorization); err != nil {
					return nil, err
				}
			}
		} else {
			if cacheContent, err = m.requestAndSaveMeta(repoType, orgRepo, commit, commitSha, method, authorization); err != nil {
				return nil, err
			}
		}
	} else {
		if cacheContent, err = m.fileDao.ReadCacheRequest(apiMetaPath); err != nil {
			zap.S().Errorf("ReadCacheRequest err.%v", err)
			return nil, err
		}
	}
	return cacheContent, nil
}

func (m *MetaDao) requestAndSaveMeta(repoType, orgRepo, commit, commitSha, method, authorization string) (*common.CacheContent, error) {
	resp, err := m.fileDao.RemoteRequestMeta(method, repoType, orgRepo, commit, authorization)
	if err != nil {
		zap.S().Errorf("requestAndSaveMeta %s err.%v", method, err)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTemporaryRedirect {
		return nil, myerr.NewAppendCode(resp.StatusCode, "request err")
	}
	extractHeaders := resp.ExtractHeaders(resp.Headers)
	apiMainDir := fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.Repos(), repoType, orgRepo, commit)
	apiMainMetaPath := fmt.Sprintf("%s/%s", apiMainDir, fmt.Sprintf("meta_%s.json", method))
	err = util.MakeDirs(apiMainMetaPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", apiMainDir, err)
		return nil, err
	}
	if err = m.fileDao.WriteCacheRequest(apiMainMetaPath, resp.StatusCode, extractHeaders, resp.Body); err != nil {
		zap.S().Errorf("writeCacheRequest err.%v", err)
		return nil, err
	}
	apiDir := fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.Repos(), repoType, orgRepo, commitSha)
	apiMetaPath := fmt.Sprintf("%s/%s", apiDir, fmt.Sprintf("meta_%s.json", method))
	err = util.MakeDirs(apiMetaPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", apiMetaPath, err)
		return nil, err
	}
	if err = m.fileDao.WriteCacheRequest(apiMetaPath, resp.StatusCode, extractHeaders, resp.Body); err != nil {
		zap.S().Errorf("writeCacheRequest err.%v", err)
		return nil, err
	}
	return &common.CacheContent{
		StatusCode:    resp.StatusCode,
		Headers:       extractHeaders,
		OriginContent: resp.Body,
	}, nil
}
