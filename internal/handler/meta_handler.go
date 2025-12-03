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
	"net/http"
	"strings"

	"dingospeed/internal/service"
	"dingospeed/pkg/consts"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type MetaHandler struct {
	metaService *service.MetaService
}

func NewMetaHandler(fileService *service.MetaService) *MetaHandler {
	return &MetaHandler{
		metaService: fileService,
	}
}

func (handler *MetaHandler) GetMetadataHandler(c echo.Context) error {
	repoType := c.Param("repoType")
	org := c.Param("org")
	repo := c.Param("repo")
	commit := c.Param("commit")
	method := strings.ToLower(c.Request().Method)
	orgRepo := util.GetOrgRepo(org, repo)
	c.Set(consts.PromOrgRepo, orgRepo)
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		zap.S().Errorf("repoType:%s is not exist RepoTypesMapping", repoType)
		return util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		zap.S().Errorf("org and repo is null")
		return util.ErrorRepoNotFound(c)
	}
	authorization := c.Request().Header.Get("authorization")
	cacheContent, err := handler.metaService.GetMetadata(repoType, orgRepo, commit, method, authorization)
	if err != nil {
		if e, ok := err.(myerr.Error); ok {
			return util.ErrorEntryUnknown(c, e.StatusCode(), e.Error())
		}
		return util.ErrorProxyError(c)
	}
	if cacheContent != nil {
		if method == consts.RequestTypeHead {
			return util.ResponseHeaders(c, http.StatusOK, cacheContent.Headers)
		}
		var bodyStreamChan = make(chan []byte, consts.RespChanSize)
		bodyStreamChan <- cacheContent.OriginContent
		close(bodyStreamChan)
		err = util.ResponseStream(c, orgRepo, cacheContent.Headers, bodyStreamChan)
		if err != nil {
			return err
		}
	}
	return nil
}

func (handler *MetaHandler) WhoamiV2Handler(c echo.Context) error {
	return handler.metaService.WhoamiV2(c)
}

func (handler *MetaHandler) ReposHandler(c echo.Context) error {
	return handler.metaService.Repos(c)
}

func (handler *MetaHandler) RepoRefsHandler(c echo.Context) error {
	repoType := c.Param("repoType")
	org := c.Param("org")
	repo := c.Param("repo")
	return handler.metaService.RepoRefs(c, repoType, org, repo)
}

func (handler *MetaHandler) ForwardToNewSiteHandler(c echo.Context) error {
	return handler.metaService.ForwardToNewSite(c)
}

func (handler *MetaHandler) RepositoryFilesHandler(c echo.Context) error {
	repoType := c.Param("repoType")
	org := c.Param("org")
	repo := c.Param("repo")
	commit := c.Param("commit")
	filePath := c.Param("filePath")
	orgRepo := util.GetOrgRepo(org, repo)
	c.Set(consts.PromOrgRepo, orgRepo)
	if _, ok := consts.RepoTypesMapping[repoType]; !ok {
		zap.S().Errorf("MetaProxyCommon repoType:%s is not exist RepoTypesMapping", repoType)
		return util.ErrorPageNotFound(c)
	}
	if org == "" && repo == "" {
		zap.S().Errorf("MetaProxyCommon org and repo is null")
		return util.ErrorRepoNotFound(c)
	}
	files, err := handler.metaService.RepositoryFiles(repoType, orgRepo, commit, filePath)
	if err != nil {
		return util.ResponseError(c, err)
	}
	return util.ResponseData(c, files)
}
