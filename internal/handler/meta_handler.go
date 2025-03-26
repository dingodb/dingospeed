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
	"strings"

	"dingo-hfmirror/internal/service"

	"github.com/labstack/echo/v4"
)

type MetaHandler struct {
	metaService *service.MetaService
}

func NewMetaHandler(fileService *service.MetaService) *MetaHandler {
	return &MetaHandler{
		metaService: fileService,
	}
}

func (handler *MetaHandler) MetaProxyCommonHandler(c echo.Context) error {
	repoType := c.Param("repoType")
	org := c.Param("org")
	repo := c.Param("repo")
	commit := c.Param("commit")
	method := strings.ToLower(c.Request().Method)
	return handler.metaService.MetaProxyCommon(c, repoType, org, repo, commit, method)
}
