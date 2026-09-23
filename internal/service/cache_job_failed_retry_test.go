package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/model/query"
	"dingospeed/internal/service/task"
	"dingospeed/pkg/app"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"github.com/labstack/echo/v4"
)

func TestModelScopeFailedTaskRetriesPinnedCache(t *testing.T) {
	payload := bytes.Repeat([]byte("retry-data-123456"), 1024)
	oid := fmt.Sprintf("%x", sha256.Sum256(payload))
	var bodies, resumedOffset atomic.Int64
	var recoverSource atomic.Bool
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			if recoverSource.Load() && r.URL.Query().Get("Revision") != "fixed" {
				t.Errorf("retry changed pinned revision: %s", r.URL)
			}
			fmt.Fprintf(w, `{"Code":200,"Data":{"LatestCommitter":{"Id":"fixed"},"Files":[{"Type":"blob","Path":"model.bin","Size":%d,"Sha256":%q,"Revision":"fixed"}]}}`, len(payload), oid)
			return
		}
		bodies.Add(1)
		if r.URL.Query().Get("Revision") != "fixed" {
			t.Error("file request changed commit")
		}
		start, end, err := util.FileRange(r.Header.Get("Range"), int64(len(payload)))
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(end-start))
		if start > 0 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(payload)))
			w.WriteHeader(206)
		}
		if !recoverSource.Load() {
			w.Write(payload[:4096])
			w.(http.Flusher).Flush()
			select {
			case <-gate:
			case <-r.Context().Done():
			}
			return // Deliberate short body after the first blocks have reached disk.
		}
		resumedOffset.Store(start)
		w.Write(payload[start:end])
	}))
	defer upstream.Close()
	defer release()
	msConfig(t, upstream.URL)
	config.SysConfig.Download.RemoteFileRangeSize = 0
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
	address, _ := url.Parse(server.URL)
	config.SysConfig.Server.Host = address.Hostname()
	config.SysConfig.Server.Port, _ = strconv.Atoi(address.Port())
	svc := NewCacheJobService(nil, nil, nil, nil)
	defer svc.cachePool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	caller := func() echo.Context {
		req := httptest.NewRequest("POST", "/", nil).WithContext(app.NewContext(ctx, app.New(app.Context(ctx))))
		return echo.New().NewContext(req, httptest.NewRecorder())
	}
	first, err := svc.CreateCacheJobResult(caller(), &query.CreateCacheJobReq{Type: consts.CacheTypePreheat, Namespace: key.Namespace, Datatype: key.RepoType, Repo: key.Repo})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := svc.CacheJobStatus(first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if status.CachedBytes >= 4096 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no cached blocks: %+v", status)
		}
		time.Sleep(time.Millisecond)
	}
	release()
	waitCacheJob(t, svc, first.ID, "failed")
	failed, _ := svc.CacheJobStatus(first.ID)
	if !strings.Contains(failed.Error, "model.bin") || !strings.Contains(failed.Error, "upstream response ended") || failed.CachedBytes < 4096 {
		t.Fatalf("failure lost cause or cache: %+v", failed)
	}
	for {
		if _, active := svc.cachePool.GetTask(int(first.ID)); !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed worker still owns task")
		}
		time.Sleep(time.Millisecond)
	}
	recoverSource.Store(true)
	if err := svc.ResumeCacheJob(caller(), &query.ResumeCacheJobReq{Id: first.ID}); err != nil {
		t.Fatal(err)
	}
	waitCacheJob(t, svc, first.ID, "complete")
	final, _ := svc.CacheJobStatus(first.ID)
	if final.Commit != "fixed" || !final.Local || final.InstanceID != "" || final.Generation != failed.Generation+1 || final.Error != "" || final.CachedBytes != uint64(len(payload)) || resumedOffset.Load() != 4096 || bodies.Load() != 2 {
		t.Fatalf("retry changed identity or lost cached blocks: %+v offset=%d requests=%d", final, resumedOffset.Load(), bodies.Load())
	}
	actual, _ := msPayload(t, key, oid)
	if !bytes.Equal(actual, payload) {
		t.Fatal("retried cached file differs from source")
	}
	t.Logf("failed -> complete; task=%d commit=%s generations=%d->%d resumedOffset=%d sha256=%s", first.ID, final.Commit, failed.Generation, final.Generation, resumedOffset.Load(), oid)
}

func TestFailedTaskResumeKeepsAdmissionGuards(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	svc := NewCacheJobService(nil, nil, nil, nil)
	defer svc.cachePool.Close()
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "owner/model"}
	for _, tc := range []struct{ state, commit, want string }{
		{"failed", "", "missing_pinned_commit"},
		{"complete", "fixed", "invalid_task_state"},
		{"canceled", "fixed", "invalid_task_state"},
	} {
		if err := writeCacheJobStatus(CacheJobStatus{ID: 991, RepoKey: key, State: tc.state, Commit: tc.commit, Local: true}); err != nil {
			t.Fatal(err)
		}
		err := svc.ResumeCacheJob(nil, &query.ResumeCacheJobReq{Id: 991})
		var conflict *CacheJobConflict
		if !errors.As(err, &conflict) || conflict.Code != tc.want {
			t.Fatalf("state=%s error=%v", tc.state, err)
		}
	}
	if err := writeCacheJobStatus(CacheJobStatus{ID: 991, RepoKey: key, State: "failed", Commit: "fixed", Local: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	active := &task.PreheatCacheTask{CacheTask: task.CacheTask{TaskNo: 992, Ctx: ctx, CancelFunc: cancel, LocalJob: true}, Sha: &dao.CommitHfSha{Sha: "fixed"}}
	if err := trackCacheJob(active, key, context.Background()); err != nil {
		t.Fatal(err)
	}
	defer active.OnFinish(context.Canceled)
	err := svc.ResumeCacheJob(nil, &query.ResumeCacheJobReq{Id: 991})
	var conflict *CacheJobConflict
	if !errors.As(err, &conflict) || conflict.Code != "active_job_conflict" || conflict.ActiveJobID != 992 {
		t.Fatalf("failed task bypassed single-writer guard: %v", err)
	}
}
