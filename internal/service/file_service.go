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
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
	"github.com/patrickmn/go-cache"
	"go.uber.org/zap"
)

type FileService struct {
	fileDao     *dao.FileDao
	commitCache *cache.Cache
}

func NewFileService(fileDao *dao.FileDao) *FileService {
	return &FileService{
		fileDao:     fileDao,
		commitCache: cache.New(10*time.Minute, 20*time.Minute),
	}
}

func (d *FileService) FileHeadCommon(c echo.Context, repoType, org, repo, commit, filePath string) error {
	orgRepo := fmt.Sprintf("%s/%s", org, repo)
	c.Set(consts.PromOrgRepo, orgRepo)
	cacheKey := fmt.Sprintf("commitSha:%s:%s:%s:%s", repoType, org, repo, commit)
	if cached, found := d.commitCache.Get(cacheKey); found {
		commitSha, ok := cached.(string)
		if ok {
			return d.fileDao.FileGetGenerator(c, repoType, orgRepo, commitSha, filePath, consts.RequestTypeHead)
		}
	}

	commitSha, err := d.getFileCommitSha(c, repoType, org, repo, commit)
	if err != nil {
		return err
	}

	d.commitCache.Set(cacheKey, commitSha, cache.DefaultExpiration)
	return d.fileDao.FileGetGenerator(c, repoType, orgRepo, commitSha, filePath, consts.RequestTypeHead)
}

func (d *FileService) FileGetCommon(c echo.Context, repoType, org, repo, commit, filePath string) error {
	zap.S().Infof("exec file get:%s/%s/%s/%s/%s, remoteAdd:%s", repoType, org, repo, commit, filePath, c.Request().RemoteAddr)
	cacheKey := fmt.Sprintf("commitSha:%s:%s:%s:%s", repoType, org, repo, commit)

	// 检查缓存
	orgRepo := util.GetOrgRepo(org, repo)
	if cached, found := d.commitCache.Get(cacheKey); found {
		if commitSha, ok := cached.(string); ok {
			zap.S().Debugf("cache hit for commitSha: %s", commitSha)
			return d.fileDao.FileGetGenerator(c, repoType, orgRepo, commitSha, filePath, consts.RequestTypeGet)
		}
		zap.S().Warnf("cache type mismatch for key: %s", cacheKey)
	}

	commitSha, err := d.getFileCommitSha(c, repoType, org, repo, commit)
	if err != nil {
		return err
	}

	d.commitCache.Set(cacheKey, commitSha, cache.DefaultExpiration)
	return d.fileDao.FileGetGenerator(c, repoType, orgRepo, commitSha, filePath, consts.RequestTypeGet)
}

func (d *FileService) getFileCommitSha(c echo.Context, repoType, org, repo, commit string) (string, error) {
	// 参数校验（保持原有逻辑不变）
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		zap.S().Errorf("getFileCommitSha repoType:%s is not exist RepoTypesMapping", repoType)
		return "", util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		zap.S().Errorf("getFileCommitSha org and repo are null")
		return "", util.ErrorRepoNotFound(c)
	}

	// 生成缓存键
	cacheKey := fmt.Sprintf("commitSha:%s:%s:%s:%s", repoType, org, repo, commit)

	// 检查缓存
	if cached, found := d.commitCache.Get(cacheKey); found {
		if commitSha, ok := cached.(string); ok {
			zap.S().Debugf("cache hit for commitSha: %s (key: %s)", commitSha, cacheKey)
			return commitSha, nil
		}
		zap.S().Warnf("cache type mismatch for key: %s", cacheKey)
	}

	// 缓存未命中，执行原始逻辑
	authorization := c.Request().Header.Get("authorization")

	if config.SysConfig.Online() {
		if code, err := d.fileDao.CheckCommitHf(repoType, org, repo, commit, authorization); err != nil {
			zap.S().Errorf("getFileCommitSha CheckCommitHf failed, commit:%s, error:%v", commit, err)
			return "", util.ErrorEntryUnknown(c, code, err.Error())
		}
	}

	commitSha, err := d.fileDao.GetCommitHf(repoType, org, repo, commit, authorization)
	if err != nil {
		zap.S().Errorf("getFileCommitSha GetCommitHf error: %v", err)
		return "", util.ErrorRepoNotFound(c)
	}

	d.commitCache.Set(cacheKey, commitSha, cache.DefaultExpiration)
	zap.S().Debugf("stored commitSha in cache: %s (key: %s)", commitSha, cacheKey)

	return commitSha, nil
}
