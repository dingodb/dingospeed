package dao

import (
	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestPeerFileRequestPreservesNamespaceAndSeparatesCredentials(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	config.SysConfig = &config.Config{Server: config.ServerConfig{}, Download: config.Download{ReqTimeout: 2}}
	key := repository.RepoKey{Namespace: "用户#50%", RepoType: "models", Repo: "team/resolve/model-a"}
	p := &downloader.TaskParam{RepoKey: key, Revision: "commit-one", FileName: "weights/a b.bin"}
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.Header.Get("X-Dingo-Service-Token") != "" {
			t.Error("internal token reached upstream")
		}
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/repositories/models/用户#50%/file" {
			t.Errorf("peer path %s", r.URL.Path)
		}
		if r.URL.Query().Get("repo") != key.Repo || r.URL.Query().Get("path") != p.FileName || r.URL.Query().Get("revision") != p.Revision {
			t.Errorf("peer locator %s", r.URL.RawQuery)
		}
		if r.Header.Get("X-Dingo-Service-Token") != "" {
			t.Error("unexpected service credential")
		}
		http.Redirect(w, r, upstream.URL, 302)
	}))
	defer peer.Close()
	if _, err := url.Parse(peerFileURI(p)); err != nil {
		t.Fatal(err)
	}
	if err := util.GetPeerStream(peer.URL, peerFileURI(p), map[string]string{"Authorization": "Bearer upstream-test"}, func(r *http.Response) error {
		if r.StatusCode != 302 {
			t.Errorf("peer redirect followed: %d", r.StatusCode)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if upstreamCalls != 0 {
		t.Fatal("peer redirect must not receive service token")
	}
	if err := util.GetStream(upstream.URL, "/", map[string]string{"X-Dingo-Service-Token": "must-be-stripped"}, func(r *http.Response) error { _, err := io.Copy(io.Discard, r.Body); return err }); err != nil {
		t.Fatal(err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls %d", upstreamCalls)
	}
}
