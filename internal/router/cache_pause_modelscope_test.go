package router

import (
	"context"
	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/model/query"
	"dingospeed/internal/service"
	"dingospeed/pkg/app"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"fmt"
	"github.com/labstack/echo/v4"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestModelScopePauseJoinsLoopbackCacheHandler(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			fmt.Fprint(w, `{"Code":200,"Data":{"LatestCommitter":{"Id":"fixed"},"Files":[{"Name":"model.bin","Path":"model.bin","Type":"blob","Size":8192,"Sha256":"file-oid","Revision":"fixed"}]}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/repo") {
			w.Header().Set("Content-Length", "8192")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(entered)
			<-r.Context().Done()
			close(canceled)
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()
	read, _ := namespaceEngines(t)
	config.SysConfig.Server.Online = true
	config.SysConfig.Download.RemoteFileRangeSize = 16384
	config.SysConfig.Modelscope = config.Modelscope{OfficialBaseURL: upstream.URL, MaxRetry: 1}
	loopback := httptest.NewServer(read)
	defer loopback.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(loopback.URL, "http://"))
	config.SysConfig.Server.Host = host
	config.SysConfig.Server.Port, _ = strconv.Atoi(port)
	base := data.NewBaseData()
	locks := dao.NewLockDao(base)
	downloads := dao.NewDownloaderDao(nil)
	files := dao.NewFileDao(downloads, base, locks)
	svc := service.NewCacheJobService(files, dao.NewMetaDao(files, locks, base), downloads, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("POST", "/", nil).WithContext(app.NewContext(ctx, app.New(app.Context(ctx))))
	id, err := svc.CreateCacheJob(echo.New().NewContext(request, httptest.NewRecorder()), &query.CreateCacheJobReq{Type: consts.CacheTypePreheat, Namespace: "modelscope", Datatype: "models", Repo: "owner/model"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	if err := svc.StopCacheJob(&query.JobStatusReq{Id: id}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := svc.CacheJobStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State == "paused" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not join cache handler: %+v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("source still active after paused confirmation")
	}
}
