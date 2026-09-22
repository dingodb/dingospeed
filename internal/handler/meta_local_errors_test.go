package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/service"
	"dingospeed/pkg/config"
	"dingospeed/pkg/dependency"
	"github.com/labstack/echo/v4"
)

func TestLocalMetadataHTTPErrorContract(t *testing.T) {
	const body = `{"sha":"abc123","id":"org/repo"}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(502) }))
	defer upstream.Close()
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	for _, method := range []string{"GET", "HEAD"} {
		for _, tc := range []struct {
			name   string
			status int
			cached bool
		}{
			{"success", 200, false}, {"missing", 404, false}, {"stat", 503, false}, {"read", 503, false}, {"corrupt", 500, false},
			{"success", 200, true}, {"missing", 404, true}, {"stat", 503, true}, {"read", 503, true}, {"corrupt", 500, true},
		} {
			name := tc.name
			if tc.cached {
				name += "-cached-sha"
			}
			t.Run(method+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				oldMonitor := dependency.Default
				dependency.Default = &dependency.Monitor{}
				t.Cleanup(func() { dependency.Default = oldMonitor })
				if tc.name == "stat" {
					root = "invalid\x00repos"
				}
				config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: root, HfScheme: "http", HfNetLoc: strings.TrimPrefix(upstream.URL, "http://")}, Retry: config.Retry{Attempts: 1}}
				base := data.NewBaseData()
				lock := dao.NewLockDao(base)
				file := dao.NewFileDao(nil, base, lock)
				if tc.cached {
					base.Cache.Set(dao.GetMetaShaRepoKey("models/huggingface/org/repo", "main", ""), "abc123", time.Minute)
				}
				writeCache := func(revision, verb string) {
					p := filepath.Join(root, "api/models/org/repo/revision", revision, "meta_"+verb+".json")
					if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
						t.Fatal(err)
					}
					if err := file.WriteCacheRequest(p, 200, map[string]string{"content-type": "application/json"}, []byte(body)); err != nil {
						t.Fatal(err)
					}
				}
				if tc.name == "success" {
					writeCache("main", "get")
					writeCache("abc123", strings.ToLower(method))
				}
				if tc.name == "read" || tc.name == "corrupt" {
					revision, verb := "main", "get"
					if tc.cached {
						revision = "abc123"
						verb = strings.ToLower(method)
					}
					p := filepath.Join(root, "api/models/org/repo/revision", revision, "meta_"+verb+".json")
					if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
						t.Fatal(err)
					}
					if tc.name == "read" {
						if err := os.Mkdir(p, 0700); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(p, []byte("bad-json"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				h := NewMetaHandler(service.NewMetaService(file, dao.NewMetaDao(file, lock, base)))
				e := echo.New()
				// The native HF route identifies the provider explicitly; avoid
				// unrelated namespace registry I/O masking metadata failures.
				e.Add(method, "/api/:repoType/:org/:repo/revision/:revision", func(c echo.Context) error { c.Set("forcedProvider", "huggingface"); return h.GetMetadataHandler(c) })
				r := httptest.NewRecorder()
				e.ServeHTTP(r, httptest.NewRequest(method, "/api/models/org/repo/revision/main", nil))
				if r.Code != tc.status {
					t.Fatalf("status=%d want=%d body=%s", r.Code, tc.status, r.Body.String())
				}
				if calls.Load() != 0 {
					t.Fatal("local metadata failure triggered upstream")
				}
				observation := dependency.Default.Snapshot(dependency.MetadataRead, time.Now())
				if tc.status == 503 && !observation.Unresolved {
					t.Fatal("storage failure missing from heartbeat observation")
				}
				if tc.status == 200 && observation.LastObservation.IsZero() {
					t.Fatal("successful cached-SHA read missing from recovery observation")
				}
				if (tc.status == 404 || tc.name == "corrupt") && observation.Unresolved {
					t.Fatal("non-storage error incorrectly marked as storage failure")
				}
				if method == "GET" && tc.status == 200 && r.Body.String() != body {
					t.Fatalf("normal payload changed: %s", r.Body.String())
				}
				if tc.status >= 500 && (strings.Contains(r.Body.String(), root) || strings.Contains(r.Body.String(), "meta_get.json")) {
					t.Fatal("internal path leaked")
				}
			})
		}
	}
}
