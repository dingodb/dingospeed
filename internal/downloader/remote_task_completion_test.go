package downloader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
)

func TestRemoteTaskCacheCompletion(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	for _, tc := range []struct {
		name                                   string
		size                                   int64
		chunk                                  int64
		failWrite, truncated, canceled, cached bool
		failLookup, oversized                  bool
	}{
		{name: "full_blocks", size: 8, chunk: 4},
		{name: "tail", size: 7, chunk: 4},
		{name: "multiple_blocks_per_chunk", size: 15, chunk: 32},
		{name: "already_cached", size: 7, chunk: 4, cached: true},
		{name: "full_block_write_failure", size: 8, chunk: 4, failWrite: true},
		{name: "tail_write_failure", size: 3, chunk: 4, failWrite: true},
		{name: "block_lookup_failure", size: 8, chunk: 4, failLookup: true},
		{name: "oversized", size: 7, chunk: 32, oversized: true},
		{name: "truncated", size: 8, chunk: 4, truncated: true},
		{name: "canceled", size: 8, chunk: 4, canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.SysConfig = &config.Config{}
			config.SysConfig.Download.RespChunkSize = tc.chunk
			config.SysConfig.Retry.Attempts = 1
			config.SysConfig.Scheduler.Mode = consts.SchedulerModeCluster
			config.SysConfig.Scheduler.OriginMode = consts.SchedulerModeCluster
			config.SysConfig.Scheduler.Strategy.SyncProcessInterval = 1
			data.NewBaseData() // Initializes in-memory reporting only; no scheduler connection.
			cache, err := NewDingCache(filepath.Join(t.TempDir(), "cache"), 4)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			if err = cache.Resize(tc.size); err != nil {
				t.Fatal(err)
			}
			payload := "abcdefghijklmno"[:tc.size]
			if tc.cached {
				for b := int64(0); b < cache.getBlockNumber(); b++ {
					block := make([]byte, 4)
					copy(block, payload[b*4:min((b+1)*4, tc.size)])
					if err = cache.WriteBlock(b, block); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.failWrite {
				cache.path = filepath.Join(t.TempDir(), "missing", "cache")
			}
			if tc.failLookup {
				cache.header.BlockMask = NewBitset(0)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if tc.oversized {
					_, _ = w.Write([]byte(payload + "extra"))
					return
				}
				if tc.truncated {
					w.Header().Set("Content-Length", "8")
					_, _ = w.Write([]byte("abcd"))
					return
				}
				_, _ = w.Write([]byte(payload))
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), consts.KeyProcessId, int64(1)))
			defer cancel()
			if tc.canceled {
				cancel()
			}
			task := NewRemoteFileTask(0, 0, tc.size)
			task.Context, task.Cancel, task.DingFile = ctx, cancel, cache
			task.Domain, task.Uri = server.URL, "/file"
			task.Queue = make(chan []byte, 32)
			done := make(chan struct{})
			go func() { task.DoTask(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("task did not terminate")
			}
			var received []byte
			for chunk := range task.Queue {
				received = append(received, chunk...)
			}
			if !tc.truncated && !tc.canceled && !tc.oversized {
				if string(received) != payload {
					t.Fatalf("delivery = %q, want %q", received, payload)
				}
				if ctx.Err() != nil {
					t.Fatalf("cache failure canceled delivery: %v", ctx.Err())
				}
			}
			want := int32(consts.StatusDownloaded)
			if tc.failWrite || tc.failLookup || tc.truncated || tc.canceled || tc.oversized {
				want = consts.StatusDownloadBreak
			}
			terminal := 0
			for len(data.GetFileProcessChan()) > 0 {
				report := <-data.GetFileProcessChan()
				if report.Status != consts.StatusDownloading {
					terminal++
					if report.Status != want {
						t.Fatalf("terminal = %d, want %d", report.Status, want)
					}
				} else if tc.failWrite || tc.failLookup {
					t.Fatal("failed write reported as cached progress")
				}
			}
			if terminal != 1 {
				t.Fatalf("terminal reports = %d", terminal)
			}
			if want == consts.StatusDownloaded {
				for b := int64(0); b < cache.getBlockNumber(); b++ {
					block, err := cache.ReadBlock(b)
					if err != nil {
						t.Fatal(err)
					}
					expected := payload[b*4 : min((b+1)*4, tc.size)]
					if string(block[:len(expected)]) != expected {
						t.Fatalf("block %d = %q", b, block)
					}
				}
			}
		})
	}
}

func TestFailedHeaderCommitDoesNotPublishBlock(t *testing.T) {
	cache, err := NewDingCache(filepath.Join(t.TempDir(), "cache"), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err = cache.Resize(4); err != nil {
		t.Fatal(err)
	}
	originalPath := cache.path
	cache.path = filepath.Join(t.TempDir(), "missing", "cache")
	cache.fileLock.Lock()
	cache.headerLock.Lock()
	err = cache.commitHeaderBlock(0)
	cache.headerLock.Unlock()
	cache.fileLock.Unlock()
	if err == nil {
		t.Fatal("expected header write failure")
	}
	if has, err := cache.HasBlock(0); err != nil || has {
		t.Fatalf("failed header commit published block: has=%v err=%v", has, err)
	}
	cache.path = originalPath
	if err = cache.WriteBlock(0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if has, err := cache.HasBlock(0); err != nil || !has {
		t.Fatalf("retry did not publish block: has=%v err=%v", has, err)
	}
}
