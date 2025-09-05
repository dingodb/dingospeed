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
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"dingospeed/internal/model"
	"dingospeed/internal/service"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/prom"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

type FileHandler struct {
	fileService *service.FileService
	sysService  *service.SysService
}

func NewFileHandler(fileService *service.FileService, sysService *service.SysService) *FileHandler {
	return &FileHandler{
		fileService: fileService,
		sysService:  sysService,
	}
}

func (handler *FileHandler) HeadFileHandler1(c echo.Context) error {
	repoType, orgRepo, commit, filePath, err := paramProcess(c, 1)
	if err != nil {
		zap.S().Error("解码出错:%v", err)
		return util.ErrorRequestParam(c)
	}
	return handler.fileService.FileHeadCommon(c, repoType, orgRepo, commit, filePath)
}

func (handler *FileHandler) HeadFileHandler2(c echo.Context) error {
	repoType, orgRepo, commit, filePath, err := paramProcess(c, 2)
	if err != nil {
		zap.S().Error("解码出错:%v", err)
		return util.ErrorRequestParam(c)
	}
	return handler.fileService.FileHeadCommon(c, repoType, orgRepo, commit, filePath)
}

func (handler *FileHandler) HeadFileHandler3(c echo.Context) error {
	repoType, orgRepo, commit, filePath, err := paramProcess(c, 3)
	if err != nil {
		zap.S().Error("解码出错:%v", err)
		return util.ErrorRequestParam(c)
	}
	return handler.fileService.FileHeadCommon(c, repoType, orgRepo, commit, filePath)
}

func (handler *FileHandler) GetFileHandler1(c echo.Context) error {
	repoType, orgRepo, commit, filePath, err := paramProcess(c, 1)
	if err != nil {
		zap.S().Error("解码出错:%v", err)
		return util.ErrorRequestParam(c)
	}
	return handler.fileGetCommon(c, repoType, orgRepo, commit, filePath)
}

func (handler *FileHandler) GetFileHandler2(c echo.Context) error {
	repoType, orgRepo, commit, filePath, err := paramProcess(c, 2)
	if err != nil {
		zap.S().Error("解码出错:%v", err)
		return util.ErrorRequestParam(c)
	}
	return handler.fileGetCommon(c, repoType, orgRepo, commit, filePath)
}

func (handler *FileHandler) GetFileHandler3(c echo.Context) error {
	repoType, orgRepo, commit, filePath, err := paramProcess(c, 3)
	if err != nil {
		zap.S().Error("解码出错:%v", err)
		return util.ErrorRequestParam(c)
	}
	return handler.fileGetCommon(c, repoType, orgRepo, commit, filePath)
}

func paramProcess(c echo.Context, processMode int) (string, string, string, string, error) {
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
		panic("param process error.")
	}
	orgRepo := util.GetOrgRepo(org, repo)
	c.Set(consts.PromOrgRepo, orgRepo)

	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		zap.S().Errorf("FileGetCommon repoType:%s is not exist RepoTypesMapping", repoType)
		return repoType, orgRepo, commit, filePath, fmt.Errorf("repoType:%s is invalid", repoType)
	}
	if org == "" && repo == "" {
		zap.S().Errorf("FileGetCommon or and repo is null")
		return repoType, orgRepo, commit, filePath, fmt.Errorf("FileGetCommon or and repo is null")
	}
	filePath, err := url.QueryUnescape(filePath)
	return repoType, orgRepo, commit, filePath, err
}

func (handler *FileHandler) fileGetCommon(c echo.Context, repoType, orgRepo, commit, filePath string) error {
	if config.SysConfig.EnableMetric() {
		labels := prometheus.Labels{}
		labels[repoType] = orgRepo
		source := util.Itoa(c.Get(consts.PromSource))
		if _, ok := consts.RepoTypesMapping[repoType]; ok {
			labels["source"] = source
			if repoType == "models" {
				prom.RequestModelCnt.With(labels).Inc()
			} else if repoType == "datasets" {
				prom.RequestDataSetCnt.With(labels).Inc()
			}
		}
		err := handler.fileService.FileGetCommon(c, repoType, orgRepo, commit, filePath)
		if _, ok := consts.RepoTypesMapping[repoType]; ok {
			labels["source"] = source
			if repoType == "models" {
				prom.RequestModelCnt.With(labels).Dec()
			} else if repoType == "datasets" {
				prom.RequestDataSetCnt.With(labels).Dec()
			}
		}
		return err
	} else {
		return handler.fileService.FileGetCommon(c, repoType, orgRepo, commit, filePath)
	}
}

func (handler *FileHandler) GetPathInfoHandler(c echo.Context) error {
	query := new(model.PathInfoQuery)
	if err := c.Bind(query); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "无效的 JSON 数据",
		})
	}
	info, err := handler.fileService.GetPathInfo(query)
	if err != nil {
		return util.ErrorProxyError(c)
	}
	return util.ResponseData(c, info)
}

func (handler *FileHandler) GetFileOffset(c echo.Context) error {
	dataType := c.Param("dataType")
	org := c.Param("org")
	repo := c.Param("repo")
	etag := c.Param("etag")
	fileSizeStr := c.Param("fileSize")

	fileSize, _ := strconv.ParseInt(fileSizeStr, 10, 64)

	offset := handler.fileService.GetFileOffset(c, dataType, org, repo, etag, fileSize)
	return util.ResponseData(c, offset)
}
