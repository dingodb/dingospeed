package downloader

import (
	"context"
	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// Regression test: a real HTTP client receives Content-Length bytes while
// the final cache write is held behind the existing file lock. No mocked context.
func TestHTTPDisconnectAfterFullDelivery(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	for _, disconnect := range []bool{false, true} {
		name := "keep_connection"
		if disconnect {
			name = "close_after_full_body"
		}
		t.Run(name, func(t *testing.T) {
			config.SysConfig = &config.Config{}
			config.SysConfig.Download.RespChunkSize = 8
			config.SysConfig.Retry.Attempts = 1
			config.SysConfig.Scheduler.Mode = consts.SchedulerModeCluster
			config.SysConfig.Scheduler.OriginMode = consts.SchedulerModeCluster
			data.NewBaseData()
			cache, err := NewDingCache(filepath.Join(t.TempDir(), "blob"), 16)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			if err = cache.Resize(7); err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("abcdefg")) }))
			defer upstream.Close()
			cache.fileLock.Lock()
			locked := true
			defer func() {
				if locked {
					cache.fileLock.Unlock()
				}
			}()
			taskDone := make(chan struct{})
			requestCanceled := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(context.WithValue(r.Context(), consts.KeyProcessId, int64(99)))
				defer cancel()
				go func() { <-ctx.Done(); close(requestCanceled) }()
				task := NewRemoteFileTask(0, 0, 7)
				task.Context = ctx
				task.Cancel = cancel
				task.DingFile = cache
				task.Domain = upstream.URL
				task.Uri = "/file"
				task.Queue = make(chan []byte, 32)
				go func() { task.DoTask(); close(taskDone) }()
				w.Header().Set("Content-Length", "7")
				for b := range task.Queue {
					_, _ = w.Write(b)
					w.(http.Flusher).Flush()
				}
				<-taskDone // handler deliberately stays alive while final cache commit waits
			}))
			defer server.Close()
			tr := &http.Transport{DisableKeepAlives: disconnect}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(body) != "abcdefg" {
				t.Fatalf("body=%q err=%v", body, err)
			}
			if disconnect {
				select {
				case <-requestCanceled:
				case <-time.After(3 * time.Second):
					t.Fatal("client close did not cancel request")
				}
			}
			cache.fileLock.Unlock()
			locked = false
			select {
			case <-taskDone:
			case <-time.After(3 * time.Second):
				t.Fatal("task did not finish")
			}
			has, err := cache.HasBlock(0)
			if err != nil || !has {
				t.Fatalf("cache incomplete: %v %v", has, err)
			}
			block, err := cache.ReadBlock(0)
			if err != nil || string(block[:7]) != "abcdefg" {
				t.Fatalf("disk mismatch %v", err)
			}
			select {
			case progress := <-data.GetFileProcessChan():
				want := int32(consts.StatusDownloaded)
				t.Logf("all 7 response bytes verified; persisted cache verified; client_disconnect=%v; reported_status=%d", disconnect, progress.Status)
				if progress.Status != want {
					t.Fatalf("diagnosed status=%d want=%d", progress.Status, want)
				}
			case <-time.After(time.Second):
				t.Fatal("no report")
			}
		})
	}
}
