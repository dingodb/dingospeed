package router

import (
	"testing"

	"dingospeed/internal/handler"
	"dingospeed/pkg/config"

	"github.com/labstack/echo/v4"
)

const localSnapshotRoute = "/api/local-repositories/:repoType/:org/:repo/revisions/:revision"

func TestLocalRepositoryRoutesAreOptIn(t *testing.T) {
	oldConfig := config.SysConfig
	t.Cleanup(func() { config.SysConfig = oldConfig })

	hasRoute := func(enabled bool) bool {
		config.SysConfig = &config.Config{Server: config.ServerConfig{LocalRepositoryAPI: enabled}}
		e := echo.New()
		NewHttpRouter(e, handler.NewFileHandler(nil, nil, nil), handler.NewMetaHandler(nil), handler.NewSysHandler(nil), handler.NewCacheJobHandler(nil), handler.NewModelscopeHandler(nil))
		for _, route := range e.Routes() {
			if route.Method == "GET" && route.Path == localSnapshotRoute {
				return true
			}
		}
		return false
	}

	if hasRoute(false) {
		t.Fatal("local repository API route is enabled by default")
	}
	if !hasRoute(true) {
		t.Fatal("local repository API route is missing when enabled")
	}
}
