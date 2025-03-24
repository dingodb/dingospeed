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
	"dingo-hfmirror/internal/dao"
	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/consts"
	"dingo-hfmirror/pkg/util"

	"github.com/labstack/echo/v4"
)

type DownloadService struct {
	downloadDao *dao.DownloadDao
}

func NewDownloadService(downloadDao *dao.DownloadDao) *DownloadService {
	return &DownloadService{
		downloadDao: downloadDao,
	}
}

func (d *DownloadService) FileHeadCommon(c echo.Context, repoType, org, repo, commit, filePath string) error {
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		return util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		return util.ErrorRepoNotFound(c)
	}
	authorization := c.Request().Header.Get("authorization")
	if config.SysConfig.Online() {
		if !d.downloadDao.CheckCommitHf(repoType, org, repo, commit, authorization) {
			return util.ErrorRepoNotFound(c)
		}
	}
	sha := d.downloadDao.GetCommitHf(repoType, org, repo, commit, authorization)
	if sha != "" {
		return util.ErrorRepoNotFound(c)
	}
	err := d.downloadDao.FileGetGenerator(repoType, org, repo, commit, filePath, c.Request())
	return err
}
