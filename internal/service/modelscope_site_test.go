package service

import (
	"context"
	"crypto/sha256"
	"dingospeed/internal/model/query"
	"dingospeed/pkg/app"
	"dingospeed/pkg/consts"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"github.com/labstack/echo/v4"
)

type siteTransport func(*http.Request) (*http.Response, error)

func TestModelScopeInternationalLiveMetadataAndFile(t *testing.T) {
	if os.Getenv("MODELSCOPE_LIVE_TEST") != "1" {
		t.Skip("set MODELSCOPE_LIVE_TEST=1 for public international upstream check")
	}
	msConfig(t, "https://www.modelscope.cn")
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "@ai/AI-ModelScope/faster-whisper-small"}
	provider := NewModelscopeService()
	ctx := echo.New().NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	meta, err := provider.RepositoryMetadata(ctx, key, "master")
	if err != nil {
		t.Fatal(err)
	}
	if meta["repo"] != key.Repo || meta["sha"] == "" {
		t.Fatalf("invalid upstream metadata: %v", meta)
	}
	w := httptest.NewRecorder()
	ctx = echo.New().NewContext(httptest.NewRequest("GET", "/", nil), w)
	if err = provider.RepositoryFile(ctx, key, "master", "README.md"); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || w.Body.Len() == 0 {
		t.Fatalf("file status=%d bytes=%d", w.Code, w.Body.Len())
	}
	t.Logf("International metadata and README download succeeded: commit=%v bytes=%d", meta["sha"], w.Body.Len())
}

func (f siteTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestModelScopeSitesKeepIdentityAndCredentialsSeparate(t *testing.T) {
	msConfig(t, "https://www.modelscope.cn")
	client := util.CreateHTTPClient()
	old := client.Transport
	t.Cleanup(func() { client.Transport = old })
	for _, site := range []string{"cn", "ai"} {
		key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "owner/model"}
		if site == "ai" {
			key.Repo = "@ai/" + key.Repo
		}
		client.Transport = siteTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "www.modelscope."+site || r.URL.Path != "/api/v1/models/owner/model/repo/files" {
				t.Fatalf("wrong origin: %s", r.URL)
			}
			if site == "ai" && (r.Header.Get("Authorization") != "Bearer ai-token" || r.Header.Get("Cookie") != "") {
				t.Fatal("cross-site credential leak")
			}
			if site == "cn" && r.Header.Get("Authorization") != "Bearer cn-token" {
				t.Fatal("domestic credentials changed")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"Code":200,"Data":{"LatestCommitter":{"Id":"same-commit"},"Files":[]}}`))}, nil
		})
		config.SysConfig.Modelscope.InternationalToken = "ai-token"
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer cn-token")
		req.Header.Set("Cookie", "cn-session")
		meta, err := NewModelscopeService().RepositoryMetadata(echo.New().NewContext(req, httptest.NewRecorder()), key, "master")
		if err != nil {
			t.Fatal(err)
		}
		if meta["repo"] != key.Repo {
			t.Fatalf("lost site: %v", meta)
		}
		if _, err = repository.Read(config.SysConfig.Repos(), key); err != nil {
			t.Fatal(err)
		}
	}
	cn := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "owner/model"}
	ai := cn
	ai.Repo = "@ai/" + cn.Repo
	if cn.Revision(config.SysConfig.Repos(), "same-commit") == ai.Revision(config.SysConfig.Repos(), "same-commit") {
		t.Fatal("shared manifest identity")
	}
	for _, repo := range []string{"@ai/owner", "@ai/owner/model/extra"} {
		if _, _, err := modelscopeOrigin(repo); err == nil {
			t.Fatalf("accepted %s", repo)
		}
	}
}

func TestModelScopeInternationalPreheatAndSiteIsolation(t *testing.T) {
	const payload = "modelscope payload"
	oid := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
	var bodies atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			fmt.Fprintf(w, `{"Code":200,"Data":{"LatestCommitter":{"Id":"ms-commit"},"Files":[{"Type":"blob","Path":"model.bin","Size":%d,"Sha256":%q,"Revision":"ms-commit"}]}}`, len(payload), oid)
			return
		}
		bodies.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		fmt.Fprint(w, payload)
	}))
	defer upstream.Close()
	msConfig(t, upstream.URL)
	client := util.CreateHTTPClient()
	oldTransport := client.Transport
	t.Cleanup(func() { client.Transport = oldTransport })
	localURL, _ := url.Parse(upstream.URL)
	client.Transport = siteTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "@ai") {
			t.Errorf("internal site leaked into upstream path: %s", r.URL)
		}
		if r.URL.Host == "www.modelscope.ai" {
			clone := r.Clone(r.Context())
			copied := *r.URL
			clone.URL = &copied
			clone.URL.Scheme, clone.URL.Host = localURL.Scheme, localURL.Host
			clone.Host = localURL.Host
			return oldTransport.RoundTrip(clone)
		}
		return oldTransport.RoundTrip(r)
	})
	config.SysConfig.Scheduler.Mode = "standalone"
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "owner/model"}
	provider := NewModelscopeService()
	e := echo.New()
	e.GET("/api/repositories/models/modelscope/metadata", func(c echo.Context) error {
		requestKey := key
		requestKey.Repo = c.QueryParam("repo")
		meta, err := provider.RepositoryMetadata(c, requestKey, c.QueryParam("revision"))
		if err != nil {
			return err
		}
		return c.JSON(200, meta)
	})
	e.GET("/api/repositories/models/modelscope/file", func(c echo.Context) error {
		requestKey := key
		requestKey.Repo = c.QueryParam("repo")
		return provider.RepositoryFile(c, requestKey, c.QueryParam("revision"), c.QueryParam("path"))
	})
	server := httptest.NewServer(e)
	defer server.Close()
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.SysConfig.Server.Host = address.Hostname()
	config.SysConfig.Server.Port, err = strconv.Atoi(address.Port())
	if err != nil {
		t.Fatal(err)
	}
	svc := NewCacheJobService(nil, nil, nil, nil)
	defer svc.cachePool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	create := func() CacheJobCreateResult {
		req := httptest.NewRequest("POST", "/", nil).WithContext(app.NewContext(ctx, app.New(app.Context(ctx))))
		result, err := svc.CreateCacheJobResult(echo.New().NewContext(req, httptest.NewRecorder()), &query.CreateCacheJobReq{Type: consts.CacheTypePreheat, Namespace: key.Namespace, Datatype: key.RepoType, Repo: key.Repo})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := create()
	waitCacheJob(t, svc, first.ID, "complete")
	domesticID := first.ID
	key.Repo = "@ai/owner/model"
	first = create()
	waitCacheJob(t, svc, first.ID, "complete")
	if first.ID == domesticID {
		t.Fatal("same commit across sites reused the wrong task")
	}
	before := bodies.Load()
	second := create()
	if second.ID != first.ID || second.Disposition != "cached" || bodies.Load() != before {
		t.Fatalf("ModelScope repeated download: first=%+v second=%+v bodies=%d/%d", first, second, before, bodies.Load())
	}
}
