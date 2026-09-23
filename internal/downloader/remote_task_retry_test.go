package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"dingospeed/pkg/config"
)

func TestRemoteDownloadRetriesFromReceivedOffset(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	for _, failure := range []string{"timeout", "short-body", "service-unavailable"} {
		t.Run(failure, func(t *testing.T) {
			config.SysConfig = &config.Config{Retry: config.Retry{Attempts: 3}, Download: config.Download{RespChunkSize: 4}}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if n == 1 && failure == "service-unavailable" {
					w.WriteHeader(503)
					return
				}
				if n == 1 {
					w.Header().Set("Content-Length", "8")
					fmt.Fprint(w, "abcd")
					w.(http.Flusher).Flush()
					if failure == "timeout" {
						<-r.Context().Done()
					}
					return
				}
				if failure == "service-unavailable" {
					fmt.Fprint(w, "abcdefgh")
					return
				}
				if r.Header.Get("Range") != "bytes=4-7" {
					t.Errorf("wrong retry Range: %q", r.Header.Get("Range"))
				}
				w.Header().Set("Content-Length", "4")
				w.Header().Set("Content-Range", "bytes 4-7/8")
				w.WriteHeader(206)
				fmt.Fprint(w, "efgh")
			}))
			defer server.Close()
			cache, err := NewDingCache(filepath.Join(t.TempDir(), "blob"), 4)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			if err := cache.Resize(8); err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Timeout: 100 * time.Millisecond}
			r := NewRemoteFileTask(0, 0, 8)
			r.Context, r.DingFile, r.Domain, r.Uri = context.Background(), cache, server.URL, "/file"
			r.Source = &RemoteSource{Fetch: func(ctx context.Context, domain, uri string, headers map[string]string, consume func(*http.Response) error) error {
				req, _ := http.NewRequestWithContext(ctx, "GET", domain+uri, nil)
				for k, v := range headers {
					req.Header.Set(k, v)
				}
				resp, err := client.Do(req)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				return consume(resp)
			}}
			chunks := make(chan []byte, 32)
			err = r.getFileRangeFromRemote(0, 8, chunks)
			close(chunks)
			var body []byte
			for chunk := range chunks {
				body = append(body, chunk...)
			}
			if err != nil || string(body) != "abcdefgh" || calls.Load() != 2 {
				t.Fatalf("calls=%d body=%q error=%v", calls.Load(), body, err)
			}
		})
	}
}
