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

package service

import (
	"fmt"
	"net/http"

	"dingospeed/internal/dao"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type MetaService struct {
	fileDao *dao.FileDao
	metaDao *dao.MetaDao
}

func NewMetaService(fileDao *dao.FileDao, metaDao *dao.MetaDao) *MetaService {
	return &MetaService{
		fileDao: fileDao,
		metaDao: metaDao,
	}
}

func (m *MetaService) MetaProxyCommon(c echo.Context, repoType, orgRepo, commit, method string) error {
	zap.S().Debugf("MetaProxyCommon:%s/%s/%s/%s", repoType, orgRepo, commit, method)
	var (
		cacheContent *common.CacheContent
		err          error
	)
	commitSha, err := m.fileDao.GetFileCommitSha(c, repoType, orgRepo, commit)
	if err != nil {
		if e, ok := err.(myerr.Error); ok {
			return util.ErrorEntryUnknown(c, e.StatusCode(), e.Error())
		}
		return util.ErrorProxyError(c)
	}
	apiDir := fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.Repos(), repoType, orgRepo, commitSha)
	apiMetaPath := fmt.Sprintf("%s/%s", apiDir, fmt.Sprintf("meta_%s.json", method))
	if config.SysConfig.Online() {
		if util.FileExists(apiMetaPath) {
			if cacheContent, err = m.fileDao.ReadCacheRequest(apiMetaPath); err != nil {
				zap.S().Errorf("ReadCacheRequest err.%v", err)
				if cacheContent, err = m.requestAndSaveMeta(c, repoType, orgRepo, commit, commitSha, method); err != nil {
					return err
				}
			}
		} else {
			if cacheContent, err = m.requestAndSaveMeta(c, repoType, orgRepo, commit, commitSha, method); err != nil {
				return err
			}
		}
	} else {
		if cacheContent, err = m.fileDao.ReadCacheRequest(apiMetaPath); err != nil {
			zap.S().Errorf("ReadCacheRequest err.%v", err)
			return util.ErrorProxyError(c)
		}
	}
	if cacheContent != nil {
		if method == consts.RequestTypeHead {
			return util.ResponseHeaders(c, cacheContent.Headers)
		}
		var bodyStreamChan = make(chan []byte, consts.RespChanSize)
		bodyStreamChan <- cacheContent.OriginContent
		close(bodyStreamChan)
		err = util.ResponseStream(c, orgRepo, cacheContent.Headers, bodyStreamChan)
		if err != nil {
			return err
		}
	} else {
		return util.ErrorProxyError(c)
	}
	return nil
}

func (m *MetaService) requestAndSaveMeta(c echo.Context, repoType, orgRepo, commit, commitSha, method string) (*common.CacheContent, error) {
	request := c.Request()
	authorization := request.Header.Get("authorization")
	resp, err := m.fileDao.RemoteRequestMeta(method, repoType, orgRepo, commit, authorization)
	if err != nil {
		zap.S().Errorf("%s err.%v", method, err)
		return nil, util.ErrorEntryNotFound(c)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTemporaryRedirect {
		return nil, util.ErrorEntryUnknown(c, resp.StatusCode, "request err")
	}
	extractHeaders := resp.ExtractHeaders(resp.Headers)

	apiMainDir := fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.Repos(), repoType, orgRepo, commit)
	apiMainMetaPath := fmt.Sprintf("%s/%s", apiMainDir, fmt.Sprintf("meta_%s.json", method))
	err = util.MakeDirs(apiMainMetaPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", apiMainDir, err)
		return nil, util.ErrorProxyError(c)
	}
	if err = m.fileDao.WriteCacheRequest(apiMainMetaPath, resp.StatusCode, extractHeaders, resp.Body); err != nil {
		zap.S().Errorf("writeCacheRequest err.%v", err)
		return nil, util.ErrorProxyError(c)
	}
	apiDir := fmt.Sprintf("%s/api/%s/%s/revision/%s", config.SysConfig.Repos(), repoType, orgRepo, commitSha)
	apiMetaPath := fmt.Sprintf("%s/%s", apiDir, fmt.Sprintf("meta_%s.json", method))
	err = util.MakeDirs(apiMetaPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", apiMetaPath, err)
		return nil, util.ErrorProxyError(c)
	}
	if err = m.fileDao.WriteCacheRequest(apiMetaPath, resp.StatusCode, extractHeaders, resp.Body); err != nil {
		zap.S().Errorf("writeCacheRequest err.%v", err)
		return nil, util.ErrorProxyError(c)
	}
	return &common.CacheContent{
		StatusCode:    resp.StatusCode,
		Headers:       extractHeaders,
		OriginContent: resp.Body,
	}, nil
}

func (m *MetaService) WhoamiV2(c echo.Context) error {
	err := m.fileDao.WhoamiV2Generator(c)
	return err
}

func (m *MetaService) Repos(c echo.Context) error {
	err := m.fileDao.ReposGenerator(c)
	return err
}

func (m *MetaService) RepoRefs(c echo.Context, repoType, org, repo string) error {
	orgRepo := util.GetOrgRepo(org, repo)
	zap.S().Debugf("RepoRefs:%s/%s", repoType, orgRepo)
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		zap.S().Errorf("RepoRefs repoType:%s is not exist RepoTypesMapping", repoType)
		return util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		zap.S().Errorf("RepoRefs org and repo is null")
		return util.ErrorRepoNotFound(c)
	}
	authorization := c.Request().Header.Get("authorization")
	localRefsDir := fmt.Sprintf("%s/api/%s/%s/refs", config.SysConfig.Repos(), repoType, orgRepo)
	localRefsPath := fmt.Sprintf("%s/%s", localRefsDir, fmt.Sprintf("refs_get.json"))
	err := util.MakeDirs(localRefsPath)
	if err != nil {
		zap.S().Errorf("create %s dir err.%v", localRefsPath, err)
		return util.ErrorProxyError(c)
	}
	var cacheContent *common.CacheContent
	if !config.SysConfig.Online() && util.FileExists(localRefsPath) {
		cacheContent, err = m.fileDao.ReadCacheRequest(localRefsPath)
		if err != nil {
			zap.S().Errorf("ReadCacheRequest %s dir err.%v", localRefsPath, err)
			return util.ErrorProxyError(c)
		}
	} else {
		resp, err := m.metaDao.RepoRefs(repoType, orgRepo, authorization)
		if err != nil {
			zap.S().Errorf("get repo refs err.%v", err)
			return util.ErrorProxyError(c)
		}
		extractHeaders := resp.ExtractHeaders(resp.Headers)
		if err = m.fileDao.WriteCacheRequest(localRefsPath, resp.StatusCode, extractHeaders, resp.Body); err != nil {
			zap.S().Errorf("writeCacheRequest err.%v", err)
			return util.ErrorProxyError(c)
		}
		cacheContent = &common.CacheContent{
			Headers:       extractHeaders,
			OriginContent: resp.Body,
		}
	}
	var bodyStreamChan = make(chan []byte, consts.RespChanSize)
	bodyStreamChan <- cacheContent.OriginContent
	close(bodyStreamChan)
	return util.ResponseStream(c, orgRepo, cacheContent.Headers, bodyStreamChan)
}

func (m *MetaService) ForwardToNewSite(c echo.Context) error {
	req := c.Request()
	zap.S().Infof("ForwardToNewSite url:%s", req.URL.Path)
	resp, err := m.metaDao.ForwardRefs(req)
	if err != nil {
		zap.S().Errorf("forward request refs err.%v", err)
		return util.ErrorProxyError(c)
	}
	extractHeaders := resp.ExtractHeaders(resp.Headers)
	cacheContent := &common.CacheContent{
		Headers:       extractHeaders,
		OriginContent: resp.Body,
	}

	var bodyStreamChan = make(chan []byte, consts.RespChanSize)
	bodyStreamChan <- cacheContent.OriginContent
	close(bodyStreamChan)
	return util.ResponseStream(c, req.URL.Path, cacheContent.Headers, bodyStreamChan)
}
