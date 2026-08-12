package router

import (
	"dingospeed/internal/handler"

	"github.com/labstack/echo/v4"
)

type UploadRouter struct {
	echo          *echo.Echo
	uploadHandler *handler.UploadHandler
}

type UploadEcho struct {
	*echo.Echo
}

func NewUploadRouter(uploadEcho UploadEcho, uploadHandler *handler.UploadHandler) *UploadRouter {
	r := &UploadRouter{echo: uploadEcho.Echo, uploadHandler: uploadHandler}
	r.initRouter()
	return r
}

func (r *UploadRouter) initRouter() {
	r.echo.GET("/api/local-upload-progress/:repoType/:org/:repo/:revision/*", r.uploadHandler.QueryProgress)
	r.echo.POST("/api/local-upload/:repoType/:org/:repo/:revision/*", r.uploadHandler.UploadWholeFile)
}

func (r *UploadRouter) Echo() *echo.Echo {
	return r.echo
}
