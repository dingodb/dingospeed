package util

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"dingospeed/pkg/config"
	myerr "dingospeed/pkg/error"
)

func TestDownloadRetryPolicy(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{Retry: config.Retry{Attempts: 3}}
	for _, tc := range []struct {
		name  string
		err   error
		calls int
	}{
		{"http-timeout", fmt.Errorf("body: %w", context.DeadlineExceeded), 3},
		{"short-body", io.ErrUnexpectedEOF, 3},
		{"connection-reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, 3},
		{"temporary-http", myerr.NewAppendCode(503, "unavailable"), 3},
		{"rate-limit", myerr.NewAppendCode(429, "busy"), 3},
		{"unauthorized", myerr.NewAppendCode(401, "unauthorized"), 1},
		{"forbidden", myerr.NewAppendCode(403, "forbidden"), 1},
		{"missing-file", myerr.NewAppendCode(404, "not found"), 1},
		{"disk-full", &os.PathError{Op: "write", Path: "cache", Err: syscall.ENOSPC}, 1},
		{"permission", os.ErrPermission, 1},
		{"canceled", context.Canceled, 1},
		{"invalid-range", errors.New("source ignored Range request"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := RetryDownloadContext(context.Background(), func() error { calls++; return tc.err })
			if !errors.Is(err, tc.err) || calls != tc.calls {
				t.Fatalf("calls=%d want=%d error=%v", calls, tc.calls, err)
			}
		})
	}
	config.SysConfig.Retry.Attempts = 0
	calls := 0
	if err := RetryDownloadContext(context.Background(), func() error { calls++; return io.ErrUnexpectedEOF }); err == nil || calls != 1 {
		t.Fatalf("zero configuration: calls=%d error=%v", calls, err)
	}
}

func TestDownloadRetryHTTPClientTimeoutWithLiveParent(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{Retry: config.Retry{Attempts: 2}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	calls := 0
	err := RetryDownloadContext(context.Background(), func() error {
		calls++
		resp, err := client.Get(server.URL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 2 {
		t.Fatalf("client timeout did not retry: calls=%d error=%v", calls, err)
	}
}

func TestDownloadRetryStopsDuringBackoff(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{Retry: config.Retry{Attempts: 3, Delay: 30}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan struct{})
	done := make(chan error, 1)
	calls := 0
	go func() {
		done <- RetryDownloadContext(ctx, func() error {
			calls++
			if calls == 1 {
				close(first)
			}
			return context.DeadlineExceeded
		})
	}()
	<-first
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("calls=%d error=%v", calls, err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for backoff")
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := RetryDownloadContext(ctx, func() error { t.Fatal("expired parent made a request"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestCacheFailureReasonDoesNotExposeProviderCredentials(t *testing.T) {
	err := fmt.Errorf("https://private.invalid/file?token=secret: %w", context.DeadlineExceeded)
	reason := CacheFailureReason(err)
	if !strings.Contains(reason, "timed out") || strings.Contains(reason, "secret") || strings.Contains(reason, "private.invalid") {
		t.Fatalf("unsafe or unhelpful reason %q", reason)
	}
}
