package service

import (
	"context"
	"crypto/sha256"
	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/model/query"
	"dingospeed/pkg/app"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/hfprojection"
	"fmt"
	"github.com/labstack/echo/v4"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreheatPauseResumeClosesHTTPAndReusesPinnedBlocks(t *testing.T) {
	for i := 0; i < 3; i++ {
		t.Run(fmt.Sprint(i), testPreheatPauseResume)
	}
}
func testPreheatPauseResume(t *testing.T) {
	testPreheatPauseResumeRegistration(t, false)
}

func TestPreheatResumeAfterJoiningScheduler(t *testing.T) {
	testPreheatPauseResumeRegistration(t, true)
}

func testPreheatPauseResumeRegistration(t *testing.T, joinScheduler bool) {
	payload := strings.Repeat("abcdefgh", 1024)
	var requests, resumedOffset atomic.Int64
	sourceCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			fmt.Fprint(w, `{"sha":"fixed","siblings":[{"rfilename":"model.bin"}]}`)
		case strings.Contains(r.URL.Path, "/paths-info/"):
			fmt.Fprintf(w, `[{"type":"file","path":"model.bin","oid":"file-oid","size":%d}]`, len(payload))
		case strings.Contains(r.URL.Path, "/resolve/"):
			number := requests.Add(1)
			if !strings.Contains(r.URL.Path, "/fixed/") {
				t.Error("download changed commit")
			}
			start, end := 0, len(payload)-1
			if value := r.Header.Get("Range"); value != "" {
				fmt.Sscanf(value, "bytes=%d-%d", &start, &end)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
			}
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			if start > 0 {
				w.WriteHeader(206)
			}
			if number == 1 {
				io.WriteString(w, payload[:4096])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(sourceCanceled)
				return
			}
			resumedOffset.Store(int64(start))
			io.WriteString(w, payload[start:end+1])
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	msConfig(t, server.URL)
	config.SysConfig.Server.HfScheme = "http"
	config.SysConfig.Server.HfNetLoc = strings.TrimPrefix(server.URL, "http://")
	config.SysConfig.Download.RemoteFileRangeSize = 16384
	base := data.NewBaseData()
	locks := dao.NewLockDao(base)
	downloads := dao.NewDownloaderDao(nil)
	files := dao.NewFileDao(downloads, base, locks)
	svc := NewCacheJobService(files, dao.NewMetaDao(files, locks, base), downloads, nil)
	defer svc.cachePool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	caller := func() echo.Context {
		return echo.New().NewContext(httptest.NewRequest("POST", "/", nil).WithContext(app.NewContext(ctx, app.New(app.Context(ctx)))), httptest.NewRecorder())
	}
	result, err := svc.CreateCacheJobResult(caller(), &query.CreateCacheJobReq{Type: consts.CacheTypePreheat, Namespace: "huggingface", Datatype: "models", Repo: "owner/model"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := svc.CacheJobStatus(result.ID)
		if err != nil {
			t.Fatal(err)
		}
		if status.CachedBytes >= 4096 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no progress: %+v", status)
		}
		time.Sleep(time.Millisecond)
	}
	if err := svc.StopCacheJob(&query.JobStatusReq{Id: result.ID}); err != nil {
		t.Fatal(err)
	}
	waitCacheJob(t, svc, result.ID, "paused")
	select {
	case <-sourceCanceled:
	case <-time.After(time.Second):
		t.Fatal("paused before source canceled")
	}
	paused, _ := svc.CacheJobStatus(result.ID)
	if paused.Error != "" || requests.Load() != 1 {
		t.Fatalf("paused=%+v requests=%d", paused, requests.Load())
	}
	// Pool cleanup follows completion; wait for ownership release before resuming.
	for {
		if _, ok := svc.cachePool.GetTask(int(result.ID)); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pool retained stopped worker")
		}
		time.Sleep(time.Millisecond)
	}
	if joinScheduler {
		if err := config.SysConfig.SaveRegistration(config.Registration{Enabled: true, NodeID: "node-a", Address: "scheduler:19091", Host: "speed", Port: 8090}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.ResumeCacheJob(caller(), &query.ResumeCacheJobReq{Id: result.ID}); err != nil {
		t.Fatal(err)
	}
	waitCacheJob(t, svc, result.ID, "complete")
	final, _ := svc.CacheJobStatus(result.ID)
	if !final.Local || final.InstanceID != "" {
		t.Fatalf("resume changed task authority: %+v", final)
	}
	if final.Generation != paused.Generation+1 || final.SnapshotVersion <= paused.SnapshotVersion || resumedOffset.Load() <= 0 {
		t.Fatalf("resume did not reuse blocks: %+v offset=%d", final, resumedOffset.Load())
	}
	reader := hfprojection.Reader{Root: config.SysConfig.Repos()}
	f, contents, err := reader.OpenFile("models", "owner/model", "fixed", "model.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	body, err := io.ReadAll(contents)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(body) != sha256.Sum256([]byte(payload)) {
		t.Fatal("resumed file differs from source")
	}
}
