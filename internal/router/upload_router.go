package router

import (
	"dingospeed/internal/handler"
	"dingospeed/pkg/middleware"

	"github.com/labstack/echo/v4"
)

type UploadRouter struct {
	echo              *echo.Echo
	uploadHandler     *handler.UploadHandler
	cacheAdminHandler *handler.CacheAdminHandler
}

type UploadEcho struct {
	*echo.Echo
}

func NewUploadRouter(uploadEcho UploadEcho, uploadHandler *handler.UploadHandler, cacheAdminHandler *handler.CacheAdminHandler) *UploadRouter {
	r := &UploadRouter{echo: uploadEcho.Echo, uploadHandler: uploadHandler, cacheAdminHandler: cacheAdminHandler}
	r.echo.Use(middleware.DecodeRouteParams, handler.CacheQueryGuard)
	r.initRouter()
	r.initCacheAdminRouter()
	return r
}

// initCacheAdminRouter 挂缓存管理。放在上传服务而不是下载服务上：
// 这些接口经控制台转发到上传端口，保留既有跨站写入检查。
//
// 只有 JSON 接口，没有页面：管理界面在 spinfield 控制台里（web/console 的
// 「缓存管理」页），经 ingest agent 转发到这里。dingospeed 自己不再提供页面，
// 否则同一套界面会有两份实现，各改各的。
func (r *UploadRouter) initCacheAdminRouter() {
	r.echo.GET("/api/scheduler-registration", handler.SchedulerRegistration)
	r.echo.PUT("/api/scheduler-registration", handler.SchedulerRegistration)
	r.echo.GET("/api/transfer-settings", handler.TransferSettings)
	r.echo.PUT("/api/transfer-settings", handler.TransferSettings)
	r.echo.GET("/api/cache/summary", r.cacheAdminHandler.Summary)
	r.echo.GET("/api/cache/repos", r.cacheAdminHandler.ListRepos)
	r.echo.GET("/api/cache/files", r.cacheAdminHandler.ListFiles)
	r.echo.POST("/api/cache/files/delete", r.cacheAdminHandler.DeleteFiles)
	r.echo.GET("/api/cache/orphans", r.cacheAdminHandler.ListOrphans)
	r.echo.POST("/api/cache/orphans/delete", r.cacheAdminHandler.PurgeOrphans)
}

func (r *UploadRouter) initRouter() {
	r.echo.DELETE("/api/repositories/:repoType/:namespace", r.uploadHandler.DeleteRepository, handler.RepositoryLocator(false, false))
	r.echo.GET("/api/upload-progress/:repoType/:namespace", r.uploadHandler.QueryProgress, handler.RepositoryLocator(true, true))
	r.echo.POST("/api/uploads/:repoType/:namespace", r.uploadHandler.UploadWholeFile, handler.RepositoryLocator(true, true), handler.LimitUpload)
	r.echo.PUT("/api/upload-chunks/:repoType/:namespace", r.uploadHandler.UploadChunk, handler.RepositoryLocator(true, true), handler.LimitUpload)
	r.echo.POST("/api/publish/:repoType/:namespace", r.uploadHandler.PublishFiles, handler.RepositoryLocator(false, true))
	r.echo.POST("/api/publish-tree/:repoType/:namespace", r.uploadHandler.PublishTree, handler.RepositoryLocator(false, true))
}

func (r *UploadRouter) Echo() *echo.Echo {
	return r.echo
}
