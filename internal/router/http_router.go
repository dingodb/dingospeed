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
	"dingospeed/pkg/middleware"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type HttpRouter struct {
	echo              *echo.Echo
	fileHandler       *handler.FileHandler
	metaHandler       *handler.MetaHandler
	sysHandler        *handler.SysHandler
	cacheJobHandler   *handler.CacheJobHandler
	modelscopeHandler *handler.ModelscopeHandler
}

func NewHttpRouter(echo *echo.Echo, fileHandler *handler.FileHandler, metaHandler *handler.MetaHandler,
	sysHandler *handler.SysHandler, cacheJobHandler *handler.CacheJobHandler, modelscopeHandler *handler.ModelscopeHandler) *HttpRouter {
	r := &HttpRouter{
		echo:              echo,
		fileHandler:       fileHandler,
		metaHandler:       metaHandler,
		sysHandler:        sysHandler,
		cacheJobHandler:   cacheJobHandler,
		modelscopeHandler: modelscopeHandler,
	}
	r.initRouter()
	return r
}

func (r *HttpRouter) initRouter() {
	r.echo.Pre(providerPrefix)
	r.echo.Use(middleware.DecodeRouteParams)
	r.echo.Use(handler.LimitDownload)
	// 系统信息
	r.echo.GET("/info", r.sysHandler.Info)
	if config.SysConfig.EnableMetric() {
		r.echo.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
	}
	// 内部使用
	r.routerForScheduler()
	r.routerForCacheJob()

	r.routerForSpeed()
	r.routerForModelscope()
}

func (r *HttpRouter) routerForSpeed() { // alayanew
	p := "/api/repositories/:repoType/:namespace"
	r.echo.GET(p+"/local-catalog", handler.HFLocalCatalog)
	r.echo.GET(p+"/local-manifest", handler.HFLocalManifest, handler.RepositoryLocator(false, true))
	r.echo.GET(p+"/local-file", handler.HFLocalFile, handler.RepositoryLocator(true, true))
	r.echo.GET(p+"/directories", r.metaHandler.RepositoryDirectories)
	r.echo.GET(p+"/revisions", r.metaHandler.RepositoryRevisions, handler.RepositoryLocator(false, false))
	r.echo.GET(p+"/snapshot", r.metaHandler.RepositorySnapshot, handler.RepositoryLocator(false, true))
	r.echo.GET(p+"/files", r.metaHandler.RepositoryFiles, handler.RepositoryLocator(false, true))
	r.echo.GET(p+"/offset", r.fileHandler.RepositoryOffset, handler.RepositoryLocator(false, false))
	r.echo.GET(p+"/tree", r.metaHandler.RepositoryTree, handler.RepositoryLocator(false, true))
	r.echo.GET(p+"/metadata", r.metaHandler.RepositoryMetadata, handler.RepositoryLocator(false, true))
	r.echo.HEAD(p+"/metadata", r.metaHandler.RepositoryMetadata, handler.RepositoryLocator(false, true))
	r.echo.GET(p+"/archive", r.metaHandler.RepositoryArchive, handler.RepositoryLocator(false, true))
	r.echo.HEAD(p+"/archive", r.metaHandler.RepositoryArchive, handler.RepositoryLocator(false, true))
	r.echo.GET(p+"/file", r.fileHandler.RepositoryFile, handler.RepositoryLocator(true, true))
	r.echo.HEAD(p+"/file", r.fileHandler.RepositoryFile, handler.RepositoryLocator(true, true))

	// 单个文件下载
	r.echo.HEAD("/:repoType/:org/:repo/resolve/:commit/*", r.fileHandler.HeadFileHandler1)
	r.echo.HEAD("/:orgOrRepoType/:repo/resolve/:commit/*", r.fileHandler.HeadFileHandler2)
	r.echo.HEAD("/:repo/resolve/:commit/*", r.fileHandler.HeadFileHandler3)
	r.echo.GET("/:repoType/:org/:repo/resolve/:commit/*", r.fileHandler.GetFileHandler1)
	r.echo.GET("/:orgOrRepoType/:repo/resolve/:commit/*", r.fileHandler.GetFileHandler2)
	r.echo.GET("/:repo/resolve/:commit/*", r.fileHandler.GetFileHandler3)

	// 模型&数据集元数据
	r.echo.HEAD("/api/:repoType/:org/:repo/revision/:revision", r.metaHandler.GetMetadataHandler)
	r.echo.GET("/api/:repoType/:org/:repo/revision/:revision", r.metaHandler.GetMetadataHandler)
	r.echo.GET("/api/:repoType/:org/:repo/tree/:revision", r.metaHandler.GetRepoTreeHandler)
	r.echo.GET("/api/:repoType/:org/:repo/tree/:revision/*", r.metaHandler.GetRepoTreeHandler)

	// refs
	r.echo.GET("/api/:repoType/:org/:repo/refs", r.metaHandler.ProtocolRefs)
	// r.echo.GET("/api/:repoType/:org/:repo/refs", r.metaHandler.RepoRefsHandler)  修复转发响应码，走统一转发。
	r.echo.GET("/api/whoami-v2", r.metaHandler.WhoamiV2Handler)
	r.echo.GET("/repos", r.metaHandler.ReposHandler)
	r.echo.Any("/*", r.metaHandler.ForwardToNewSiteHandler)
}

func (r *HttpRouter) routerForScheduler() {
	r.echo.GET("/api/fileProcessSync", r.fileHandler.FileProcessSync)
	r.echo.GET("/api/upload-inventory", handler.UploadInventory)
}

func (r *HttpRouter) routerForCacheJob() { // alayanew
	r.echo.GET("/api/cacheJob/status", r.cacheJobHandler.CacheJobStatus)
	r.echo.POST("/api/cacheJob/create", r.cacheJobHandler.CreateCacheJobHandler)
	r.echo.POST("/api/cacheJob/stop", r.cacheJobHandler.StopCacheJobHandler)
	r.echo.POST("/api/cacheJob/resume", r.cacheJobHandler.ResumeCacheJobHandler)
	r.echo.POST("/api/cacheJob/realtime", r.cacheJobHandler.RealtimeCacheJobHandler)
}

func (r *HttpRouter) routerForModelscope() { // modelscope
	r.echo.GET("/api/v1/:repoType/:org/:repo", r.modelscopeHandler.ModelInfoHandler)
	r.echo.GET("/api/v1/:repoType/:org/:repo/revisions", r.modelscopeHandler.RevisionsHandler)
	r.echo.GET("/api/v1/:repoType/:org/:repo/repo/files", r.modelscopeHandler.FileListHandler)
	r.echo.GET("/api/v1/:repoType/:org/:repo/repo", r.modelscopeHandler.FileDownloadHandler)
	r.echo.HEAD("/api/v1/:repoType/:org/:repo/repo", r.modelscopeHandler.FileDownloadHandler)
	r.echo.GET("/api/v1/:repoType/:org/:repo/repo/tree", r.modelscopeHandler.FileTreeHandler)
	r.echo.GET("/api/v1/datasets/:datasetId/repo/tree", r.modelscopeHandler.DatasetFileTreeHandler)
}
