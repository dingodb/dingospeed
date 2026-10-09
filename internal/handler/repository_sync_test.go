package handler

import (
	"dingospeed/pkg/config"
	"github.com/labstack/echo/v4"
	"net/http/httptest"
	"testing"
)

func TestRepositorySyncGuardRequiresClusterWithoutKey(t *testing.T) {
	t.Setenv("DINGO_SYNC_KEY", "")
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir()}, Scheduler: config.Scheduler{Mode: "cluster"}}
	e := echo.New()
	called := false
	e.GET("/", func(c echo.Context) error { called = true; return c.NoContent(200) }, RepositorySyncGuard)
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != 200 || !called {
		t.Fatalf("cluster without key: status=%d called=%t", w.Code, called)
	}
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir()}}
	called = false
	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 409 || called {
		t.Fatalf("standalone: status=%d called=%t", w.Code, called)
	}
}
