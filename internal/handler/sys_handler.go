package handler

import (
	"dingo-hfmirror/internal/service"
	"dingo-hfmirror/pkg/app"
	"dingo-hfmirror/pkg/util"

	"github.com/labstack/echo/v4"
)

type SysHandler struct {
	sysService *service.SysService
}

func NewSysHandler(sysService *service.SysService) *SysHandler {
	return &SysHandler{
		sysService: sysService,
	}
}

func (s *SysHandler) Info(c echo.Context) error {
	info := s.sysService.Info()
	if appInfo, ok := app.FromContext(c.Request().Context()); ok {
		info.Id = appInfo.ID()
		info.Name = appInfo.Name()
		info.Version = appInfo.Version()
	}
	return util.ResponseData(c, info)
}
