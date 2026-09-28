package util

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"

	"dingospeed/pkg/config"
	"dingospeed/pkg/proxypool"
)

// A safe request tries each healthy outlet at most once before direct fallback.
// The original context remains attached across every attempt and response body.
type sourceFailoverTransport struct {
	source string
	pool   *proxypool.Pool
	first  *proxypool.Member
	last   atomic.Pointer[proxypool.Member]
	failed atomic.Bool
}

func (t *sourceFailoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tried := map[*proxypool.Member]bool{}
	m := t.first
	for {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		tried[m] = true
		resp, err := m.Client(req.Method).Transport.RoundTrip(req)
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		if req.Context().Err() == nil {
			t.pool.Report(m, code, err)
		}
		if !proxypool.IsFailure(code, err) {
			t.last.Store(m)
			resp.Body = &outletBody{ReadCloser: resp.Body, req: req, transport: t}
			return resp, nil
		}
		// Never replay mutations automatically, or retry after caller cancellation.
		if (req.Method != http.MethodGet && req.Method != http.MethodHead) || req.Context().Err() != nil {
			return resp, err
		}
		next := t.pool.PickExcluding(tried)
		if next == nil && !config.SysConfig.GetProxyPoolFallbackDirect() {
			if err != nil {
				return nil, fmt.Errorf("%s proxy %s failed; direct fallback disabled: %w", t.source, m.Name(), err)
			}
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		if next != nil {
			m = next
			continue
		}
		fallback := req.Clone(req.Context())
		proxypool.FallbackDirectTotal.Inc()
		logFallbackThrottled(t.source)
		t.last.Store(nil)
		client := CreateHTTPClient()
		if t.source == "huggingface" {
			// Only rewrite the HF origin, never a redirected object-storage URL.
			primary, _ := url.Parse(config.SysConfig.GetHFURLBase())
			if req.URL.Host == primary.Host {
				base, e := url.Parse(config.SysConfig.GetBpHFURLBase())
				if e != nil {
					return nil, e
				}
				u := *req.URL
				u.Scheme, u.Host = base.Scheme, base.Host
				fallback.URL, fallback.Host = &u, ""
			}
			client, err = NewHTTPClient(req.Method)
			if err != nil {
				return nil, err
			}
		}
		transport := client.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		resp, err = transport.RoundTrip(fallback)
		if err != nil {
			return nil, fmt.Errorf("%s direct fallback %s failed: %w", t.source, fallback.URL.Host, err)
		}
		return resp, nil
	}
}

type outletBody struct {
	io.ReadCloser
	req       *http.Request
	transport *sourceFailoverTransport
}

func (t *sourceFailoverTransport) reportBody(err error) {
	if err != nil && !isClientCanceled(err) && t.failed.CompareAndSwap(false, true) {
		t.pool.Report(t.last.Load(), 0, err)
	}
}

func (b *outletBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF && b.req.Context().Err() == nil {
		b.transport.reportBody(err)
	}
	return n, err
}
