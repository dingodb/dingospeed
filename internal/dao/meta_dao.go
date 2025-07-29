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
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/util"
	"fmt"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"net/http"
	"time"
)

type MetaDao struct {
	fileDao *FileDao
}

func NewMetaDao(fileDao *FileDao) *MetaDao {
	return &MetaDao{
		fileDao: fileDao,
	}
}

func (m *MetaDao) MetaGetGenerator(c echo.Context, repoType, org, repo, commit, method string, writeResp bool) error {
	request := c.Request()
	authorization := request.Header.Get("authorization")
	orgRepo := util.GetOrgRepo(org, repo)
	apiDir := fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.Repos(), repoType, orgRepo, commit)
	apiMetaPath := fmt.Sprintf("%s/%s", apiDir, fmt.Sprintf("meta_%s.json", method))
	err := util.MakeDirs(apiMetaPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", apiDir, err)
		return err
	}
	// 若缓存文件存在，且为离线模式，从缓存读取
	if util.FileExists(apiMetaPath) && !config.SysConfig.Online() {
		return m.MetaCacheGenerator(c, repo, apiMetaPath)
	} else {
		return m.MetaProxyGenerator(c, repoType, org, repo, commit, method, authorization, apiMetaPath, writeResp)
	}
}

func (m *MetaDao) MetaCacheGenerator(c echo.Context, repo, apiMetaPath string) error {
	cacheContent, err := m.fileDao.ReadCacheRequest(apiMetaPath)
	if err != nil {
		return err
	}
	var bodyStreamChan = make(chan []byte, consts.RespChanSize)
	bodyStreamChan <- cacheContent.OriginContent
	close(bodyStreamChan)
	err = util.ResponseStream(c, repo, cacheContent.Headers, bodyStreamChan)
	if err != nil {
		return err
	}
	return nil
}

// 请求api文件

func (m *MetaDao) MetaProxyGenerator(c echo.Context, repoType, org, repo, commit, method, authorization, apiMetaPath string, writeResp bool) error {
	resp, err := m.fileDao.remoteRequestMeta(method, repoType, org, repo, commit, authorization)
	if err != nil {
		zap.S().Errorf("%s err.%v", method, err)
		return util.ErrorEntryNotFound(c)

	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTemporaryRedirect {
		return util.ErrorEntryUnknown(c, resp.StatusCode, "request err")
	}

	extractHeaders := resp.ExtractHeaders(resp.Headers)
	if method == consts.RequestTypeHead {
		return util.ResponseHeaders(c, extractHeaders)
	} else if method == consts.RequestTypeGet {
		if writeResp {
			var bodyStreamChan = make(chan []byte, consts.RespChanSize)
			bodyStreamChan <- resp.Body
			close(bodyStreamChan)
			if err = util.ResponseStream(c, repo, extractHeaders, bodyStreamChan); err != nil {
				return err
			}
		}
		if err = m.fileDao.WriteCacheRequest(apiMetaPath, resp.StatusCode, extractHeaders, resp.Body); err != nil {
			zap.S().Errorf("writeCacheRequest err.%v", err)
			return nil
		}
	}
	return nil
}

func (m *MetaDao) RepoRefs(repoType string, orgRepo string, authorization string) (*common.Response, error) {
	refsUrl := fmt.Sprintf("%s/api/%s/%s/refs", config.SysConfig.GetHFURLBase(), repoType, orgRepo)
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	resp, err := util.RetryRequest(func() (*common.Response, error) {
		return util.Get(refsUrl, headers, config.SysConfig.GetReqTimeOut())
	})
	return resp, err
}

func (m *MetaDao) ForwardRefs(targetURL string, originalReq *http.Request, timeout time.Duration) (*common.Response, error) {
	resp, err := util.RetryRequest(func() (*common.Response, error) {
		return util.ForwardRequest(targetURL, originalReq, timeout)
	})
	return resp, err
}
