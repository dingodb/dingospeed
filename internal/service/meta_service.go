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
	"dingospeed/internal/dao"
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

func (d *MetaService) MetaProxyCommon(c echo.Context, repoType, org, repo, commit, method string) error {
	zap.S().Debugf("MetaProxyCommon:%s/%s/%s/%s/%s", repoType, org, repo, commit, method)
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		zap.S().Errorf("MetaProxyCommon repoType:%s is not exist RepoTypesMapping", repoType)
		return util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		zap.S().Errorf("MetaProxyCommon or and repo is null")
		return util.ErrorRepoNotFound(c)
	}
	authorization := c.Request().Header.Get("authorization")
	if config.SysConfig.Online() {
		// check repo
		if !d.fileDao.CheckCommitHf(repoType, org, repo, "", authorization) {
			zap.S().Errorf("MetaProxyCommon CheckCommitHf is false, commit is null")
			return util.ErrorRepoNotFound(c)
		}
		// check repo commit
		if !d.fileDao.CheckCommitHf(repoType, org, repo, commit, authorization) {
			zap.S().Errorf("MetaProxyCommon CheckCommitHf is false, commit:%s", commit)
			return util.ErrorRevisionNotFound(c, commit)
		}
	}
	commitSha, err := d.fileDao.GetCommitHf(repoType, org, repo, commit, authorization)
	if err != nil {
		zap.S().Errorf("MetaProxyCommon GetCommitHf err.%v", err)
		return util.ErrorRepoNotFound(c)
	}
	if config.SysConfig.Online() && commitSha != commit {
		_ = d.metaDao.MetaGetGenerator(c, repoType, org, repo, commit, method, false)
		return d.metaDao.MetaGetGenerator(c, repoType, org, repo, commitSha, method, true)
	} else {
		return d.metaDao.MetaGetGenerator(c, repoType, org, repo, commitSha, method, true)
	}
}

func (d *MetaService) WhoamiV2(c echo.Context) error {
	err := d.fileDao.WhoamiV2Generator(c)
	return err
}

func (d *MetaService) Repos(c echo.Context) error {
	err := d.fileDao.ReposGenerator(c)
	return err
}
