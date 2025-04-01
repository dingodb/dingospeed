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
	"go.uber.org/zap"
)

type FileService struct {
	fileDao *dao.FileDao
}

func NewFileService(fileDao *dao.FileDao) *FileService {
	return &FileService{
		fileDao: fileDao,
	}
}

func (d *FileService) FileHeadCommon(c echo.Context, repoType, org, repo, commit, filePath string) error {
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		return util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		return util.ErrorRepoNotFound(c)
	}
	authorization := c.Request().Header.Get("authorization")
	if config.SysConfig.Online() {
		if !d.fileDao.CheckCommitHf(repoType, org, repo, commit, authorization) {
			return util.ErrorRepoNotFound(c)
		}
	}
	commitSha, err := d.fileDao.GetCommitHf(repoType, org, repo, commit, authorization)
	if err != nil {
		zap.S().Errorf("GetCommitHf err.%v", err)
		return util.ErrorRepoNotFound(c)
	}
	return d.fileDao.FileGetGenerator(c, repoType, org, repo, commitSha, filePath, consts.RequestTypeHead)
}

func (d *FileService) FileGetCommon(c echo.Context, repoType, org, repo, commit, filePath string) error {
	zap.S().Debugf("exec file get:%s/%s/%s/%s/%s", repoType, org, repo, commit, filePath)
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		return util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		return util.ErrorRepoNotFound(c)
	}
	authorization := c.Request().Header.Get("authorization")
	if config.SysConfig.Online() {
		if !d.fileDao.CheckCommitHf(repoType, org, repo, commit, authorization) { // 若请求找不到，直接返回仓库不存在。
			return util.ErrorRepoNotFound(c)
		}
	}
	commitSha, err := d.fileDao.GetCommitHf(repoType, org, repo, commit, authorization)
	if err != nil {
		zap.S().Errorf("GetCommitHf err.%v", err)
		return util.ErrorRepoNotFound(c)
	}
	return d.fileDao.FileGetGenerator(c, repoType, org, repo, commitSha, filePath, consts.RequestTypeGet)
}
