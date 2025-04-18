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

package router

import (
	"dingo-hfmirror/internal/handler"

	"github.com/labstack/echo/v4"
)

type HttpRouter struct {
	echo        *echo.Echo
	fileHandler *handler.FileHandler
	metaHandler *handler.MetaHandler
	sysHandler  *handler.SysHandler
}

func NewHttpRouter(echo *echo.Echo, fileHandler *handler.FileHandler, metaHandler *handler.MetaHandler, sysHandler *handler.SysHandler) *HttpRouter {
	r := &HttpRouter{
		echo:        echo,
		fileHandler: fileHandler,
		metaHandler: metaHandler,
		sysHandler:  sysHandler,
	}
	r.initRouter()
	return r
}

func (r *HttpRouter) initRouter() {
	// 系统信息
	r.echo.GET("/info", r.sysHandler.Info)

	// 单个文件
	r.echo.HEAD("/:repoType/:org/:repo/resolve/:commit/:filePath", r.fileHandler.HeadFileHandler1)
	r.echo.HEAD("/:orgOrRepoType/:repo/resolve/:commit/:filePath", r.fileHandler.HeadFileHandler2)
	r.echo.HEAD("/:repo/resolve/:commit/:filePath", r.fileHandler.HeadFileHandler3)

	r.echo.GET("/:repoType/:org/:repo/resolve/:commit/:filePath", r.fileHandler.GetFileHandler1)
	r.echo.GET("/:orgOrRepoType/:repo/resolve/:commit/:filePath", r.fileHandler.GetFileHandler2)
	r.echo.GET("/:repo/resolve/:commit/:filePath", r.fileHandler.GetFileHandler3)

	// 模型
	r.echo.HEAD("/api/:repoType/:org/:repo/revision/:commit", r.metaHandler.MetaProxyCommonHandler)
	r.echo.GET("/api/:repoType/:org/:repo/revision/:commit", r.metaHandler.MetaProxyCommonHandler)

	r.echo.GET("/api/whoami-v2", r.metaHandler.WhoamiV2Handler)
	r.echo.GET("/repos", r.metaHandler.ReposHandler)

}
