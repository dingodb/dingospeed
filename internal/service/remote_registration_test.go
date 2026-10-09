package service

import (
	"context"
	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/model/query"
	"dingospeed/pkg/app"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/repository"
	"fmt"
	"github.com/labstack/echo/v4"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRemoteMetadataRegistration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		valid  bool
	}{
		{"missing", 404, `{"Code":404,"Success":false}`, false},
		{"application-error", 200, `{"Code":200,"Success":false,"Data":{}}`, false},
		{"malformed", 200, `not json`, false},
		{"empty", 200, `{}`, false},
		{"valid-empty-revisions", 200, `{"Code":200,"Data":{"RevisionMap":{"Branches":[],"Tags":[]}}}`, true},
	} {
		for _, proxy := range []bool{false, true} {
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/proxy=%v/existing=%v", tc.name, proxy, existing), func(t *testing.T) {
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
					defer upstream.Close()
					msConfig(t, upstream.URL)
					k := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "owner/model"}
					if existing {
						if err := repository.Register(config.SysConfig.Repos(), repository.Remote(k)); err != nil {
							t.Fatal(err)
						}
					}
					svc := NewModelscopeService()
					rec := httptest.NewRecorder()
					c := echo.New().NewContext(httptest.NewRequest("GET", "/api/v1/models/owner/model/revisions", nil), rec)
					c.SetParamNames("repoType", "org", "repo")
					c.SetParamValues("models", "owner", "model")
					if proxy {
						if err := svc.ForwardRevisions(c, "owner", "model", "models"); err != nil {
							t.Fatal(err)
						}
						if rec.Code != tc.status || rec.Body.String() != tc.body {
							t.Fatalf("proxy response changed: %d %s", rec.Code, rec.Body.String())
						}
					} else {
						_, err := svc.RepositoryRevisions(c, k)
						if (err == nil) != tc.valid {
							t.Fatalf("valid=%v error=%v", tc.valid, err)
						}
					}
					_, err := repository.Read(config.SysConfig.Repos(), k)
					if existing || tc.valid {
						if err != nil {
							t.Fatal(err)
						}
					} else if !os.IsNotExist(err) {
						t.Fatalf("failed request left marker: %v", err)
					}
				})
			}
		}
	}
}

func TestFailedRemoteCacheJobDoesNotRegister(t *testing.T) {
	for _, ns := range []string{repository.HuggingFace, repository.ModelScope} {
		t.Run(ns, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(404)
				fmt.Fprint(w, `{"Code":404,"Success":false}`)
			}))
			defer upstream.Close()
			msConfig(t, upstream.URL)
			config.SysConfig.Server.HfScheme = "http"
			config.SysConfig.Server.HfNetLoc = strings.TrimPrefix(upstream.URL, "http://")
			config.SysConfig.Scheduler.Mode = "standalone"
			local := echo.New()
			ms := NewModelscopeService()
			local.GET("/api/repositories/models/modelscope/metadata", func(c echo.Context) error {
				_, err := ms.RepositoryMetadata(c, repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: c.QueryParam("repo")}, c.QueryParam("revision"))
				return err
			})
			server := httptest.NewServer(local)
			defer server.Close()
			address, _ := url.Parse(server.URL)
			config.SysConfig.Server.Host = address.Hostname()
			config.SysConfig.Server.Port, _ = strconv.Atoi(address.Port())

			base := data.NewBaseData()
			locks := dao.NewLockDao(base)
			downloads := dao.NewDownloaderDao(nil)
			files := dao.NewFileDao(downloads, base, locks)
			svc := NewCacheJobService(files, dao.NewMetaDao(files, locks, base), downloads, nil)
			defer svc.cachePool.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("POST", "/api/cacheJob/create", nil).WithContext(app.NewContext(ctx, app.New(app.Context(ctx))))
			c := echo.New().NewContext(req, httptest.NewRecorder())
			_, err := svc.CreateCacheJob(c, &query.CreateCacheJobReq{Type: consts.CacheTypePreheat, Namespace: ns, Datatype: "models", Repo: "owner/missing"})
			if err == nil {
				t.Fatal("expected metadata failure")
			}
			if requests.Load() == 0 {
				t.Fatal("upstream was never reached")
			}
			k := repository.RepoKey{Namespace: ns, RepoType: "models", Repo: "owner/missing"}
			if _, err := repository.Read(config.SysConfig.Repos(), k); !os.IsNotExist(err) {
				t.Fatalf("failed job left marker: %v", err)
			}
		})
	}
}
