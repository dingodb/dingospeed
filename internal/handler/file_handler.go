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

type FileHandler struct {
	fileService *service.FileService
}

func NewFileHandler(fileService *service.FileService) *FileHandler {
	return &FileHandler{
		fileService: fileService,
	}
}

func (handler *FileHandler) HeadFileHandler1(c echo.Context) error {
	repoType, org, repo, commit, filePath := paramProcess(c, 1)
	return handler.fileService.FileHeadCommon(c, repoType, org, repo, commit, filePath)
}

func (handler *FileHandler) HeadFileHandler2(c echo.Context) error {
	repoType, org, repo, commit, filePath := paramProcess(c, 2)
	return handler.fileService.FileHeadCommon(c, repoType, org, repo, commit, filePath)
}

func (handler *FileHandler) HeadFileHandler3(c echo.Context) error {
	repoType, org, repo, commit, filePath := paramProcess(c, 3)
	return handler.fileService.FileHeadCommon(c, repoType, org, repo, commit, filePath)
}

func paramProcess(c echo.Context, processMode int) (string, string, string, string, string) {
	var (
		repoType string
		org      string
		repo     string
		commit   string
		filePath string
	)
	if processMode == 1 {
		repoType = c.Param("repoType")
		org = c.Param("org")
		repo = c.Param("repo")
		commit = c.Param("commit")
		filePath = c.Param("filePath")
	} else if processMode == 2 {
		orgOrRepoType := c.Param("orgOrRepoType")
		repo = c.Param("repo")
		commit = c.Param("commit")
		filePath = c.Param("filePath")
		if _, ok := consts.RepoTypesMapping[orgOrRepoType]; ok {
			repoType = orgOrRepoType
			org = ""
		} else {
			repoType = "models"
			org = orgOrRepoType
		}
	} else if processMode == 3 {
		repo = c.Param("repo")
		commit = c.Param("commit")
		filePath = c.Param("filePath")
		repoType = "models"
	} else {
		panic("param error.")
	}
	return repoType, org, repo, commit, filePath
}

func (handler *FileHandler) GetFileHandler1(c echo.Context) error {
	repoType, org, repo, commit, filePath := paramProcess(c, 1)
	return handler.fileService.FileGetCommon(c, repoType, org, repo, commit, filePath)
}

func (handler *FileHandler) GetFileHandler2(c echo.Context) error {
	repoType, org, repo, commit, filePath := paramProcess(c, 2)
	return handler.fileService.FileGetCommon(c, repoType, org, repo, commit, filePath)
}

func (handler *FileHandler) GetFileHandler3(c echo.Context) error {
	repoType, org, repo, commit, filePath := paramProcess(c, 3)
	return handler.fileService.FileGetCommon(c, repoType, org, repo, commit, filePath)
}
