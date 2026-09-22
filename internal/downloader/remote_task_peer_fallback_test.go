package downloader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
)

func TestModelScopePeerFailureFallsBackToOfficialFromDownloadedOffset(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	config.SysConfig = &config.Config{}
	config.SysConfig.Download.RespChunkSize = 16
	config.SysConfig.Retry.Attempts = 1

	cache, err := NewDingCache(filepath.Join(t.TempDir(), "blob"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err = cache.Resize(10); err != nil {
		t.Fatal(err)
	}

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("abc"))
	}))
	defer peer.Close()

	var gotRange string
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("defghij"))
	}))
	defer official.Close()

	source := &RemoteSource{
		Domain: official.URL,
		Fetch: func(ctx context.Context, domain, uri string, headers map[string]string, consume func(*http.Response) error) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, domain+uri, nil)
			if err != nil {
				return err
			}
			for key, value := range headers {
				req.Header.Set(key, value)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			return consume(resp)
		},
	}
	task := NewRemoteFileTask(0, 0, 10)
	task.Context = context.Background()
	task.DingFile = cache
	task.RepoKey = repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "Qwen/demo"}
	task.OrgRepo = task.RepoKey.ID()
	task.FileName = "model.bin"
	task.Domain = peer.URL
	task.Uri = "/api/repositories/models/modelscope/file"
	task.UpstreamURI = "/api/v1/models/Qwen/demo/repo"
	task.Peer = true
	task.Source = source

	content := make(chan []byte, 2)
	if err = task.getFileRangeFromRemote(0, 10, content); err != nil {
		t.Fatal(err)
	}
	close(content)
	var body []byte
	for chunk := range content {
		body = append(body, chunk...)
	}
	if string(body) != "abcdefghij" {
		t.Fatalf("body=%q", body)
	}
	if gotRange != "bytes=3-9" {
		t.Fatalf("official Range=%q, want %q", gotRange, "bytes=3-9")
	}
	if task.Domain != official.URL || task.Uri != task.UpstreamURI || task.Peer {
		t.Fatalf("fallback state domain=%q uri=%q peer=%v", task.Domain, task.Uri, task.Peer)
	}
}
