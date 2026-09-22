package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/model/query"
	"dingospeed/pkg/app"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/repository"
	"github.com/labstack/echo/v4"
)

func waitCacheJob(t *testing.T, svc *CacheJobService, id int64, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := svc.CacheJobStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, err := svc.CacheJobStatus(id)
	t.Fatalf("want %s, got %+v %v", want, status, err)
}

func TestPreheatConcurrentRequestsReuseAndRevalidate(t *testing.T) {
	var version atomic.Int32
	version.Store(1)
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			fmt.Fprintf(w, `{"sha":"commit-%d","siblings":[{"rfilename":"model.bin"}]}`, version.Load())
		case strings.Contains(r.URL.Path, "/paths-info/"):
			fmt.Fprint(w, `[{"type":"file","path":"model.bin","oid":"file-oid","size":8}]`)
		case strings.Contains(r.URL.Path, "/resolve/"):
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Length", "8")
			fmt.Fprint(w, "abcdefgh")
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	msConfig(t, upstream.URL)
	config.SysConfig.Server.HfScheme = "http"
	config.SysConfig.Server.HfNetLoc = strings.TrimPrefix(upstream.URL, "http://")
	config.SysConfig.Scheduler.Mode = "standalone"
	base := data.NewBaseData()
	locks := dao.NewLockDao(base)
	downloads := dao.NewDownloaderDao(nil)
	files := dao.NewFileDao(downloads, base, locks)
	svc := NewCacheJobService(files, dao.NewMetaDao(files, locks, base), downloads, nil)
	defer svc.cachePool.Close()
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	create := func() (CacheJobCreateResult, error) {
		req := httptest.NewRequest("POST", "/api/cacheJob/create", nil).WithContext(app.NewContext(ctx, app.New(app.Context(ctx))))
		return svc.CreateCacheJobResult(echo.New().NewContext(req, httptest.NewRecorder()), &query.CreateCacheJobReq{Type: consts.CacheTypePreheat, Namespace: "huggingface", Datatype: "models", Repo: "owner/model"})
	}
	// Start all callers together, including the first one: admission must be atomic.
	const callers = 12
	results := make([]CacheJobCreateResult, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = create() }(i)
	}
	wg.Wait()
	created := 0
	for i, result := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if result.ID != results[0].ID {
			t.Fatalf("duplicate job IDs: %+v", results)
		}
		if result.Disposition == "created" {
			created++
		} else if result.Disposition != "running" {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if created != 1 {
		t.Fatalf("created %d jobs", created)
	}
	release()
	id := results[0].ID
	waitCacheJob(t, svc, id, "complete")
	result, err := create()
	if err != nil || result.ID != id || result.Disposition != "cached" {
		t.Fatalf("completed reuse: %+v %v", result, err)
	}

	// Removing a reference after completion must allow the cache to be filled again.
	if err := os.Remove(dao.ResolvePath("models", "huggingface/owner/model", "commit-1", "model.bin")); err != nil {
		t.Fatal(err)
	}
	result, err = create()
	if err != nil || result.ID == id || result.Disposition != "created" {
		t.Fatalf("deleted cache reuse: %+v %v", result, err)
	}
	waitCacheJob(t, svc, result.ID, "complete")
	version.Store(2)
	next, err := create()
	if err != nil || next.ID == result.ID || next.Disposition != "created" || next.Commit != "commit-2" {
		t.Fatalf("new commit: %+v %v", next, err)
	}
	waitCacheJob(t, svc, next.ID, "complete")
	// A restarted worker pool can reuse a complete persisted job, but not an orphaned runner.
	restarted := &CacheJobService{cachePool: common.NewPool(1, true)}
	defer restarted.cachePool.Close()
	metadata, err := svc.preheatMetadata(ctx, repository.RepoKey{Namespace: "huggingface", RepoType: "models", Repo: "owner/model"}, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	reused, err := restarted.reusableCacheJob(repository.RepoKey{Namespace: "huggingface", RepoType: "models", Repo: "owner/model"}, metadata, true, "")
	if err != nil || reused == nil || reused.ID != next.ID {
		t.Fatalf("restart reuse: %+v %v", reused, err)
	}
}

func TestReusablePreheatIdentityAndTerminalStates(t *testing.T) {
	for _, provider := range []string{"huggingface", "modelscope"} {
		t.Run(provider, func(t *testing.T) {
			msConfig(t, "http://unused.invalid")
			svc := &CacheJobService{cachePool: common.NewPool(1, true)}
			defer svc.cachePool.Close()
			key := repository.RepoKey{Namespace: provider, RepoType: "models", Repo: "owner/model"}
			snapshot := &dao.CommitHfSha{Sha: "commit-1"}
			for _, state := range []string{"failed", "interrupted", "stopped", "running", "waiting"} {
				if err := writeCacheJobStatus(CacheJobStatus{ID: 42, RepoKey: key, Commit: snapshot.Sha, State: state, Local: true}); err != nil {
					t.Fatal(err)
				}
				got, err := svc.reusableCacheJob(key, snapshot, true, "")
				if err != nil || got != nil {
					t.Fatalf("reused %s: %+v %v", state, got, err)
				}
			}
			if err := writeCacheJobStatus(CacheJobStatus{ID: 42, RepoKey: key, Commit: snapshot.Sha, State: "complete", InstanceID: "node-a"}); err != nil {
				t.Fatal(err)
			}
			got, err := svc.reusableCacheJob(key, snapshot, false, "node-a")
			if err != nil || got == nil {
				t.Fatalf("not reused: %+v %v", got, err)
			}
			for _, change := range []string{"node", "provider", "repo", "commit", "mode"} {
				otherKey, otherSnapshot, node, local := key, *snapshot, "node-a", false
				switch change {
				case "node":
					node = "node-b"
				case "provider":
					otherKey.Namespace = "other-provider"
				case "repo":
					otherKey.Repo = "another/model"
				case "commit":
					otherSnapshot.Sha = "commit-2"
				case "mode":
					local = true
				}
				got, err := svc.reusableCacheJob(otherKey, &otherSnapshot, local, node)
				if err != nil || got != nil {
					t.Fatalf("cross-%s reuse: %+v %v", change, got, err)
				}
			}
		})
	}
}

func TestModelScopePreheatDedupThroughRepositoryAPI(t *testing.T) {
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
	config.SysConfig.Scheduler.Mode = "standalone"
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "owner/model"}
	provider := NewModelscopeService()
	e := echo.New()
	e.GET("/api/repositories/models/modelscope/metadata", func(c echo.Context) error {
		meta, err := provider.RepositoryMetadata(c, key, c.QueryParam("revision"))
		if err != nil {
			return err
		}
		return c.JSON(200, meta)
	})
	e.GET("/api/repositories/models/modelscope/file", func(c echo.Context) error {
		return provider.RepositoryFile(c, key, c.QueryParam("revision"), c.QueryParam("path"))
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
	before := bodies.Load()
	second := create()
	if second.ID != first.ID || second.Disposition != "cached" || bodies.Load() != before {
		t.Fatalf("ModelScope repeated download: first=%+v second=%+v bodies=%d/%d", first, second, before, bodies.Load())
	}
}
