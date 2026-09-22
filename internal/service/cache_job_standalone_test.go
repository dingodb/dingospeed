package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/model/query"
	"dingospeed/pkg/app"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/proto/manager"
	"github.com/labstack/echo/v4"
)

func TestStandaloneCacheJobLifecycle(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("downloadFailure=", fail), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/revision/"):
					fmt.Fprint(w, `{"sha":"commit-one","usedStorage":24,"siblings":[{"rfilename":"model.bin"}]}`)
				case strings.Contains(r.URL.Path, "/paths-info/"):
					fmt.Fprint(w, `[{"type":"file","path":"model.bin","oid":"file-oid","size":8}]`)
				case strings.Contains(r.URL.Path, "/resolve/"):
					if fail {
						w.WriteHeader(403)
						return
					}
					w.Header().Set("Content-Length", "8")
					fmt.Fprint(w, "abcdefgh")
				default:
					t.Errorf("unexpected upstream %s", r.URL)
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
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("POST", "/api/cacheJob/create", nil).WithContext(app.NewContext(ctx, app.New(app.Context(ctx))))
			c := echo.New().NewContext(req, httptest.NewRecorder())
			id, err := svc.CreateCacheJob(c, &query.CreateCacheJobReq{Type: consts.CacheTypePreheat, Namespace: "huggingface", Datatype: "models", Repo: "owner/model"})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			var status CacheJobStatus
			for time.Now().Before(deadline) {
				status, err = svc.CacheJobStatus(id)
				if err != nil {
					t.Fatal(err)
				}
				if status.State == "complete" || status.State == "failed" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			want := "complete"
			if fail {
				want = "failed"
			}
			if status.State != want || !status.Local || status.TotalBytes != 8 {
				t.Fatalf("status=%+v", status)
			}
			if !fail && status.CachedBytes != 8 {
				t.Fatalf("missing bytes: %+v", status)
			}
			if fail && status.Error == "" {
				t.Fatal("missing failure reason")
			}
			restarted := &CacheJobService{cachePool: common.NewPool(1, true)}
			defer restarted.cachePool.Close()
			reloaded, err := restarted.CacheJobStatus(id)
			if err != nil || reloaded.State != want {
				t.Fatalf("restart lost result: %+v %v", reloaded, err)
			}
		})
	}
}

func TestCacheJobIdentityClusterAndStandalone(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	svc := &CacheJobService{}
	if local, id, err := svc.cacheJobIdentity(""); !local || id != "" || err != nil {
		t.Fatalf("standalone: %v %q %v", local, id, err)
	}
	config.SysConfig.Scheduler.Mode = consts.SchedulerModeCluster
	config.SysConfig.Scheduler.OriginMode = consts.SchedulerModeCluster
	config.SysConfig.Scheduler.Discovery.InstanceId = "speed-1"
	if err := config.SysConfig.SaveRegistration(config.Registration{Enabled: true, NodeID: "speed-1", Address: "scheduler:19091", Host: "speed", Port: 8090}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.cacheJobIdentity("speed-2"); err == nil {
		t.Fatal("mismatched binding accepted")
	}
	if _, _, err := svc.cacheJobIdentity(""); err == nil {
		t.Fatal("missing scheduler accepted")
	}
	// A degraded cluster must not silently create a standalone job.
	config.SysConfig.Scheduler.Mode = "standalone"
	if _, _, err := svc.cacheJobIdentity(""); err == nil {
		t.Fatal("degraded cluster switched task authority")
	}
	svc.schedulerDao = &dao.SchedulerDao{Client: manager.NewManagerClient(nil)}
	if local, id, err := svc.cacheJobIdentity(""); local || id != "speed-1" || err != nil {
		t.Fatalf("cluster: %v %q %v", local, id, err)
	}
}

func TestLocalCacheIDsDoNotOverwriteAfterRestart(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	seen := map[int64]bool{}
	for i := 0; i < 20; i++ {
		id, err := reserveLocalCacheJob()
		if err != nil || seen[id] {
			t.Fatalf("ID collision: %d %v", id, err)
		}
		seen[id] = true
	}
}
