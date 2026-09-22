package util

import (
	"context"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStreamCancellationClosesBlockedRequest(t *testing.T) {
	old := config.SysConfig
	config.SysConfig = &config.Config{}
	defer func() { config.SysConfig = old }()
	for _, headers := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers", true: "body"}[headers], func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if headers {
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				close(entered)
				<-r.Context().Done()
				close(canceled)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := doGetStreamContext(ctx, server.Client(), server.URL, nil, func(resp *http.Response) error { _, err := io.Copy(io.Discard, resp.Body); return err })
				done <- err
			}()
			<-entered
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("HTTP request did not stop")
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("source did not observe cancellation")
			}
		})
	}
}

func TestRetryCancellationInterruptsBackoff(t *testing.T) {
	old := config.SysConfig
	config.SysConfig = &config.Config{}
	defer func() { config.SysConfig = old }()
	config.SysConfig.Retry.Attempts = 5
	config.SysConfig.Retry.Delay = 30
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	_, err := RetryRequestContext(ctx, func() (*common.Response, error) { calls++; cancel(); return nil, errors.New("retryable failure") })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}
