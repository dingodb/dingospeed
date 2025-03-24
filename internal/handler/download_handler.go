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

package handler

import (
	"dingo-hfmirror/internal/service"
	"dingo-hfmirror/pkg/consts"

	"github.com/labstack/echo/v4"
)

type DownloadHandler struct {
	downloadService *service.DownloadService
}

func NewDownloadHandler(downloadService *service.DownloadService) *DownloadHandler {
	return &DownloadHandler{
		downloadService: downloadService,
	}
}

func (handler *DownloadHandler) HeadDownloadHandler1(c echo.Context) error {
	repoType := c.Param("repoType")
	org := c.Param("org")
	repo := c.Param("repo")
	commit := c.Param("commit")
	filePath := c.Param("filePath")
	handler.downloadService.FileHeadCommon(c, repoType, org, repo, commit, filePath)
	return nil
}

func (handler *DownloadHandler) HeadDownloadHandler2(c echo.Context) error {
	orgOrRepoType := c.Param("orgOrRepoType")
	repo := c.Param("repo")
	commit := c.Param("commit")
	filePath := c.Param("filePath")
	var repoType, org string
	if _, ok := consts.RepoTypesMapping[orgOrRepoType]; ok {
		repoType = orgOrRepoType
		org = ""
	} else {
		repoType = "models"
		org = orgOrRepoType
	}
	handler.downloadService.FileHeadCommon(c, repoType, org, repo, commit, filePath)
	return nil
}

func (handler *DownloadHandler) HeadDownloadHandler3(c echo.Context) error {
	repo := c.Param("repo")
	commit := c.Param("commit")
	filePath := c.Param("filePath")
	repoType := "models"
	handler.downloadService.FileHeadCommon(c, repoType, "", repo, commit, filePath)
	return nil
}

func (handler *DownloadHandler) GetDownloadHandler(c echo.Context) error {
	return nil
}
