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
	"time"

	"dingospeed/internal/dao"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
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

func (m *MetaService) MetaProxyCommon(c echo.Context, repoType, org, repo, commit, method string) error {
	zap.S().Debugf("MetaProxyCommon:%s/%s/%s/%s/%s", repoType, org, repo, commit, method)
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		zap.S().Errorf("MetaProxyCommon repoType:%s is not exist RepoTypesMapping", repoType)
		return util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		zap.S().Errorf("MetaProxyCommon org and repo is null")
		return util.ErrorRepoNotFound(c)
	}
	authorization := c.Request().Header.Get("authorization")
	if config.SysConfig.Online() {
		// check repo
		if code, err := m.fileDao.CheckCommitHf(repoType, org, repo, "", authorization); err != nil {
			zap.S().Errorf("MetaProxyCommon CheckCommitHf is false, commit is null")
			return util.ErrorEntryUnknown(c, code, err.Error())
		}
		// check repo commit
		if code, err := m.fileDao.CheckCommitHf(repoType, org, repo, commit, authorization); err != nil {
			zap.S().Errorf("MetaProxyCommon CheckCommitHf is false, commit:%s", commit)
			return util.ErrorEntryUnknown(c, code, err.Error())
		}
	}
	commitSha, err := m.fileDao.GetCommitHf(repoType, org, repo, commit, authorization)
	if err != nil {
		zap.S().Errorf("MetaProxyCommon GetCommitHf err.%v", err)
		return util.ErrorRepoNotFound(c)
	}
	if config.SysConfig.Online() && commitSha != commit {
		_ = m.metaDao.MetaGetGenerator(c, repoType, org, repo, commit, method, false)
		return m.metaDao.MetaGetGenerator(c, repoType, org, repo, commitSha, method, true)
	} else {
		return m.metaDao.MetaGetGenerator(c, repoType, org, repo, commitSha, method, true)
	}
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
	target, err := config.SysConfig.GetHFURL()
	if err != nil {
		return util.ErrorProxyError(c)
	}

	req := c.Request()
	zap.S().Infof("ForwardToNewSite url:%s", req.URL.Path)

	resp, err := m.metaDao.ForwardRefs(target.String(), req, 30*time.Second)
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
	return util.ResponseStream(c, target.String(), cacheContent.Headers, bodyStreamChan)
}
