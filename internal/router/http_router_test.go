package router

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"dingospeed/internal/handler"
	"dingospeed/pkg/config"
	"dingospeed/pkg/prom"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
)

func TestStorageMetricsUseExistingRouteAndFlag(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	// Instantiate an existing metric so its lazy label series is exposed too.
	prom.RequestTotalCnt.WithLabelValues("storage-metrics-test").Inc()
	t.Cleanup(func() { prom.RequestTotalCnt.DeleteLabelValues("storage-metrics-test") })
	util.ObserveFileAccessFailure("read", &os.PathError{Op: "read", Path: "private-path", Err: os.ErrPermission})
	for _, enabled := range []bool{false, true, true, false} {
		config.SysConfig = &config.Config{Server: config.ServerConfig{Metrics: enabled, Repos: "invalid\x00repos"}}
		e := echo.New()
		NewHttpRouter(e, handler.NewFileHandler(nil, nil, nil), handler.NewMetaHandler(nil), handler.NewSysHandler(nil), handler.NewCacheJobHandler(nil), handler.NewModelscopeHandler(nil))
		found := false
		for _, route := range e.Routes() {
			if route.Method == "GET" && route.Path == "/metrics" {
				found = true
			}
		}
		if found != enabled {
			t.Fatalf("metrics route enabled=%v want %v", found, enabled)
		}
		if !enabled {
			continue
		} // Existing catch-all routing is not changed.
		r := httptest.NewRecorder()
		e.ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
		if r.Code != 200 {
			t.Fatalf("scrape failed: %d %s", r.Code, r.Body.String())
		}
		body := r.Body.String()
		for _, want := range []string{
			"# TYPE dingospeed_storage_access_errors_total counter",
			`dingospeed_storage_access_errors_total{kind="permission_denied",operation="read"} 1`,
			`request_total_cnt{source="storage-metrics-test"} 1`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("missing %s", want)
			}
		}
		if strings.Contains(body, "private-path") {
			t.Fatal("path leaked to metrics")
		}
	}
}

func TestRepositoryRoutesReplaceLocalAliases(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	config.SysConfig = &config.Config{}
	e := echo.New()
	NewHttpRouter(e, handler.NewFileHandler(nil, nil, nil), handler.NewMetaHandler(nil), handler.NewSysHandler(nil), handler.NewCacheJobHandler(nil), handler.NewModelscopeHandler(nil))
	got := map[string]bool{}
	for _, r := range e.Routes() {
		if strings.Contains(r.Path, "/api/local-") {
			t.Fatalf("legacy alias remains: %s", r.Path)
		}
		got[r.Method+" "+r.Path] = true
	}
	for _, op := range []string{"snapshot", "archive", "file", "metadata", "tree", "directories", "revisions"} {
		if !got["GET /api/repositories/:repoType/:namespace/"+op] {
			t.Fatalf("missing %s", op)
		}
	}
}
