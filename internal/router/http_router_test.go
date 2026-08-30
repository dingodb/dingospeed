package router

import (
	"testing"

	"dingospeed/internal/handler"
	"dingospeed/pkg/config"

	"github.com/labstack/echo/v4"
)

const localSnapshotRoute = "/api/local-repositories/:repoType/:org/:repo/revisions/:revision"
const localArchiveRoute = localSnapshotRoute + "/archive"

func TestLocalRepositoryRoutesAreOptIn(t *testing.T) {
	oldConfig := config.SysConfig
	t.Cleanup(func() { config.SysConfig = oldConfig })

	routes := func(enabled bool) map[string]bool {
		config.SysConfig = &config.Config{Server: config.ServerConfig{LocalRepositoryAPI: enabled}}
		e := echo.New()
		NewHttpRouter(e, handler.NewFileHandler(nil, nil, nil), handler.NewMetaHandler(nil), handler.NewSysHandler(nil), handler.NewCacheJobHandler(nil), handler.NewModelscopeHandler(nil))
		got := map[string]bool{}
		for _, route := range e.Routes() {
			got[route.Method+" "+route.Path] = true
		}
		return got
	}

	if got := routes(false); got["GET "+localSnapshotRoute] || got["GET "+localArchiveRoute] || got["HEAD "+localArchiveRoute] {
		t.Fatal("local repository API route is enabled by default")
	}
	got := routes(true)
	if !got["GET "+localSnapshotRoute] || !got["GET "+localArchiveRoute] || !got["HEAD "+localArchiveRoute] {
		t.Fatal("local repository API route is missing when enabled")
	}
}
