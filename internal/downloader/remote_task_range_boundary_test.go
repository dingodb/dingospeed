package downloader

import (
	"bytes"
	"context"
	"dingospeed/pkg/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestRemoteRangeDoesNotInventFinalWholeBlock(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{Download: config.Download{RespChunkSize: 4}}
	cache, err := NewDingCache(filepath.Join(t.TempDir(), "cache"), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err = cache.Resize(41 * 4); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("abcd"), 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-159/164")
		w.Header().Set("Content-Length", "160")
		w.WriteHeader(206)
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task := NewRemoteFileTask(0, 0, 40*4)
	task.Context, task.Cancel, task.DingFile = ctx, cancel, cache
	task.Domain, task.Uri = server.URL, "/file"
	task.LocalOnly = true
	task.Queue = make(chan []byte, 64)
	task.DoTask()
	for range task.Queue {
	}
	if task.ResultError != nil {
		t.Fatal(task.ResultError)
	}
	for b := int64(0); b < 40; b++ {
		present, err := cache.HasBlock(b)
		if err != nil || !present {
			t.Fatalf("block %d not cached: %v", b, err)
		}
	}
	present, err := cache.HasBlock(40)
	if err != nil || present {
		t.Fatalf("unrequested last block fabricated: %v %v", present, err)
	}
}
