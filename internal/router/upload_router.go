package router

import (
	"dingospeed/internal/handler"

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
	r.initRouter()
	r.initCacheAdminRouter()
	return r
}

// initCacheAdminRouter 挂缓存管理。放在上传服务而不是下载服务上：
// 这些接口会删数据，必须待在只监听回环地址、且要求上传 token 的那个端口后面。
//
// 只有 JSON 接口，没有页面：管理界面在 spinfield 控制台里（web/console 的
// 「缓存管理」页），经 ingest agent 转发到这里。dingospeed 自己不再提供页面，
// 否则同一套界面会有两份实现，各改各的。
func (r *UploadRouter) initCacheAdminRouter() {
	r.echo.GET("/api/cache/summary", r.cacheAdminHandler.Summary)
	r.echo.GET("/api/cache/repos", r.cacheAdminHandler.ListRepos)
	r.echo.GET("/api/cache/files", r.cacheAdminHandler.ListFiles)
	r.echo.POST("/api/cache/files/delete", r.cacheAdminHandler.DeleteFiles)
	r.echo.GET("/api/cache/orphans", r.cacheAdminHandler.ListOrphans)
	r.echo.POST("/api/cache/orphans/delete", r.cacheAdminHandler.PurgeOrphans)
}

func (r *UploadRouter) initRouter() {
	r.echo.GET("/api/local-upload-progress/:repoType/:org/:repo/:revision/*", r.uploadHandler.QueryProgress)
	r.echo.POST("/api/local-upload/:repoType/:org/:repo/:revision/*", r.uploadHandler.UploadWholeFile)
	// 分块是幂等的（重复写同一块直接返回 already_present），用 PUT 而不是 POST。
	r.echo.PUT("/api/local-upload-chunk/:repoType/:org/:repo/:revision/*", r.uploadHandler.UploadChunk)
	r.echo.POST("/api/local-publish/:repoType/:org/:repo/:revision", r.uploadHandler.PublishFiles)
	// 整树发布：清单是目标全量而非增量，因此能表达删除。仓库编辑走这条路径，
	// 新建上传继续走 local-publish（那条做并集合并，仓库/revision 可以不存在）。
	r.echo.POST("/api/local-publish-tree/:repoType/:org/:repo/:revision", r.uploadHandler.PublishTree)
}

func (r *UploadRouter) Echo() *echo.Echo {
	return r.echo
}
