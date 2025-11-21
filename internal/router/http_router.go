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
	"dingospeed/internal/handler"
	"dingospeed/pkg/config"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type HttpRouter struct {
	echo            *echo.Echo
	fileHandler     *handler.FileHandler
	metaHandler     *handler.MetaHandler
	sysHandler      *handler.SysHandler
	cacheJobHandler *handler.CacheJobHandler
}

func NewHttpRouter(echo *echo.Echo, fileHandler *handler.FileHandler, metaHandler *handler.MetaHandler,
	sysHandler *handler.SysHandler, cacheJobHandler *handler.CacheJobHandler) *HttpRouter {
	r := &HttpRouter{
		echo:            echo,
		fileHandler:     fileHandler,
		metaHandler:     metaHandler,
		sysHandler:      sysHandler,
		cacheJobHandler: cacheJobHandler,
	}
	r.initRouter()
	return r
}

func (r *HttpRouter) initRouter() {
	// 系统信息
	r.echo.GET("/info", r.sysHandler.Info)
	if config.SysConfig.EnableMetric() {
		r.echo.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
	}
	// 内部使用
	r.routerForScheduler()
	r.routerForCacheJob()

	r.routerForSpeed()
}

func (r *HttpRouter) routerForSpeed() { // alayanew
	// 单个文件下载
	r.echo.HEAD("/:repoType/:org/:repo/resolve/:commit/:filePath", r.fileHandler.HeadFileHandler1)
	r.echo.HEAD("/:orgOrRepoType/:repo/resolve/:commit/:filePath", r.fileHandler.HeadFileHandler2)
	r.echo.HEAD("/:repo/resolve/:commit/:filePath", r.fileHandler.HeadFileHandler3)
	r.echo.GET("/:repoType/:org/:repo/resolve/:commit/:filePath", r.fileHandler.GetFileHandler1)
	r.echo.GET("/:orgOrRepoType/:repo/resolve/:commit/:filePath", r.fileHandler.GetFileHandler2)
	r.echo.GET("/:repo/resolve/:commit/:filePath", r.fileHandler.GetFileHandler3)

	// 模型&数据集元数据
	r.echo.HEAD("/api/:repoType/:org/:repo/revision/:commit", r.metaHandler.GetMetadataHandler)
	r.echo.GET("/api/:repoType/:org/:repo/revision/:commit", r.metaHandler.GetMetadataHandler)

	// refs
	// r.echo.GET("/api/:repoType/:org/:repo/refs", r.metaHandler.RepoRefsHandler)  修复转发响应码，走统一转发。
	r.echo.GET("/api/whoami-v2", r.metaHandler.WhoamiV2Handler)
	r.echo.GET("/repos", r.metaHandler.ReposHandler)
	r.echo.Any("/*", r.metaHandler.ForwardToNewSiteHandler)
}

func (r *HttpRouter) routerForScheduler() { // alayanew
	r.echo.GET("/api/:repoType/:org/:repo/files/:commit/", r.metaHandler.RepositoryFilesHandler)
	r.echo.GET("/api/:repoType/:org/:repo/files/:commit/:filePath", r.metaHandler.RepositoryFilesHandler)

	r.echo.POST("/api/getPathInfo", r.fileHandler.GetPathInfoHandler)
	r.echo.GET("/api/fileOffset/:dataType/:org/:repo/:etag/:fileSize", r.fileHandler.GetFileOffset)
	r.echo.GET("/api/fileProcessSync", r.fileHandler.FileProcessSync)

}

func (r *HttpRouter) routerForCacheJob() { // alayanew
	r.echo.POST("/api/cacheJob/create", r.cacheJobHandler.CreateCacheJobHandler)
	r.echo.POST("/api/cacheJob/stop", r.cacheJobHandler.StopCacheJobHandler)
	r.echo.POST("/api/cacheJob/resume", r.cacheJobHandler.ResumeCacheJobHandler)
	r.echo.POST("/api/cacheJob/realtime", r.cacheJobHandler.RealtimeCacheJobHandler)
}
