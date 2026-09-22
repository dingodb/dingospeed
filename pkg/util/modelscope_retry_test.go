package util

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dingospeed/pkg/config"
)

func TestModelScopeZeroRetryStillRequests(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; io.WriteString(w, "ok") }))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL, nil)
	resp, err := DoRequestWithRetry(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls != 1 || resp.StatusCode != 200 {
		t.Fatalf("initial request suppressed: calls=%d", calls)
	}
}

type msTimeoutTransport struct{ calls int }

func (t *msTimeoutTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, context.DeadlineExceeded
}

func TestModelScopeRetryDelayUsesSecondsAndCancels(t *testing.T) {
	old := config.SysConfig
	client := CreateHTTPClient()
	oldTransport := client.Transport
	transport := &msTimeoutTransport{}
	client.Transport = transport
	t.Cleanup(func() { config.SysConfig = old; client.Transport = oldTransport })
	config.SysConfig = &config.Config{Modelscope: config.Modelscope{MaxRetry: 3, RetryDelay: 1}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://modelscope.test", nil)
	_, err := DoRequestWithRetry(req)
	if !errors.Is(err, context.DeadlineExceeded) || transport.calls != 1 {
		t.Fatalf("retry did not wait for seconds/cancellation: calls=%d error=%v", transport.calls, err)
	}
}
