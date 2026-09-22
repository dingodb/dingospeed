package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/service"
	"dingospeed/pkg/config"
	"github.com/labstack/echo/v4"
)

// Online metadata cache failures do not discard successful HF responses.
// Local 404/503 and namespace validation retain their distinct behavior.
func TestMetadataStorageLegacyHTTPContract(t *testing.T) {
	const body = `{"sha":"remote-commit","id":"org/repo"}`
	var calls atomic.Int64
	var upstreamCode atomic.Int64
	upstreamCode.Store(200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-only" {
			t.Error("authorization lost")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"fixture"`)
		w.WriteHeader(int(upstreamCode.Load()))
		if r.Method != http.MethodHead {
			fmt.Fprint(w, body)
		}
	}))
	defer server.Close()
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	for _, tc := range []struct {
		name             string
		online           bool
		failure          string
		upstream, status int
		count            int64
	}{
		{"online-success", true, "", 200, 200, 2},
		{"online-mkdir-failure", true, "stat", 200, 200, 2},
		{"online-write-failure", true, "write", 200, 200, 2},
		{"online-descriptor-write-failure", true, "registration", 200, 200, 2},
		{"offline-missing", false, "", 200, 404, 0},
		{"offline-stat-failure", false, "stat", 200, 503, 0},
		{"offline-read-failure", false, "read", 200, 503, 0},
		{"upstream-denied", true, "", 403, 403, 1},
		{"upstream-missing", true, "", 404, 404, 1},
		{"upstream-unavailable", true, "", 503, 503, 1},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				repos := t.TempDir()
				if tc.failure == "registration" {
					if err := os.MkdirAll(filepath.Join(repos, "api/.repository-registration.lock"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if tc.failure == "stat" {
					repos = "invalid\x00repos"
				}
				if tc.failure == "read" || tc.failure == "write" {
					cacheMethod := strings.ToLower(method)
					if tc.failure == "read" {
						cacheMethod = "get"
					}
					path := filepath.Join(repos, "api/models/org/repo/revision/main/meta_"+cacheMethod+".json")
					if err := os.MkdirAll(path, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(path, "keep"), []byte("unchanged"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				config.SysConfig = &config.Config{Server: config.ServerConfig{Online: tc.online, Repos: repos, HfScheme: "http", HfNetLoc: strings.TrimPrefix(server.URL, "http://")}, Retry: config.Retry{Attempts: 1}}
				base := data.NewBaseData()
				lock := dao.NewLockDao(base)
				file := dao.NewFileDao(nil, base, lock)
				h := NewMetaHandler(service.NewMetaService(file, dao.NewMetaDao(file, lock, base)))
				e := echo.New()
				e.Add(method, "/api/:repoType/:org/:repo/revision/:revision", h.GetMetadataHandler)
				req := httptest.NewRequest(method, "/api/models/org/repo/revision/main", nil)
				req.Header.Set("Authorization", "Bearer test-only")
				rec := httptest.NewRecorder()
				before := calls.Load()
				upstreamCode.Store(int64(tc.upstream))
				e.ServeHTTP(rec, req)
				if rec.Code != tc.status || calls.Load()-before != tc.count {
					t.Fatalf("status=%d calls=%d; want %d/%d body=%s", rec.Code, calls.Load()-before, tc.status, tc.count, rec.Body.String())
				}
				if tc.status == 200 && method == http.MethodGet && rec.Body.String() != body {
					t.Fatalf("upstream payload changed: %s", rec.Body.String())
				}
				if tc.status == 200 && rec.Header().Get("ETag") != `"fixture"` {
					t.Fatal("upstream ETag lost")
				}
				if tc.status == 200 && method == http.MethodHead && strings.TrimSpace(rec.Body.String()) != "null" {
					t.Fatal("legacy HEAD handler serialization changed")
				}
				if tc.status == 200 && tc.failure == "" {
					if method == http.MethodGet && rec.Body.String() != body {
						t.Fatalf("body changed: %s", rec.Body.String())
					}
					for _, revision := range []string{"main", "remote-commit"} {
						cache, err := file.ReadCacheRequest(filepath.Join(repos, "api/models/org/repo/revision", revision, "meta_"+strings.ToLower(method)+".json"))
						if err != nil {
							t.Fatal(err)
						}
						if method == http.MethodGet && string(cache.OriginContent) != body {
							t.Fatal("cache payload changed")
						}
					}
				} else if tc.failure == "write" || tc.failure == "read" {
					cacheMethod := strings.ToLower(method)
					if tc.failure == "read" {
						cacheMethod = "get"
					}
					content, err := os.ReadFile(filepath.Join(repos, "api/models/org/repo/revision/main/meta_"+cacheMethod+".json/keep"))
					if err != nil || string(content) != "unchanged" {
						t.Fatal("existing cache modified")
					}
				}
			})
		}
	}
}
