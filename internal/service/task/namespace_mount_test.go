package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"dingospeed/internal/model/query"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
)

func TestCacheProgressReaderReportsBytesBeforeFileCompletes(t *testing.T) {
	var progress atomic.Uint64
	reader := &cacheProgressReader{reader: strings.NewReader("first-second"), add: progress.Add}
	buffer := make([]byte, 5)

	n, err := reader.Read(buffer)
	if err != nil || n != 5 || progress.Load() != 5 {
		t.Fatalf("first read n=%d progress=%d err=%v", n, progress.Load(), err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if progress.Load() != uint64(len("first-second")) {
		t.Fatalf("final progress=%d", progress.Load())
	}
}

func TestMountAndPreheatUseCanonicalNamespaceAndMachineToken(t *testing.T) {
	previous := config.SysConfig
	t.Cleanup(func() { config.SysConfig = previous })
	config.SysConfig = &config.Config{Server: config.ServerConfig{}, Cache: config.Cache{MountModelDir: t.TempDir()}}
	repo, name := "team/resolve/model-a", "weights/50%#.bin"
	fileRequests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != 6 || parts[1] != "api" || parts[2] != "repositories" || parts[3] != "models" {
			t.Errorf("unexpected route %q", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		ns, operation := parts[4], parts[5]
		if r.Header.Get("X-Dingo-Service-Token") != "" || r.Header.Get("Authorization") != "Bearer provider" || r.URL.Query().Get("repo") != repo {
			t.Errorf("lost credentials or repository: %s", r.URL.String())
			w.WriteHeader(401)
			return
		}
		switch operation {
		case "metadata":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"sha": "commit-1", "siblings": []map[string]string{{"rfilename": name}}})
		case "file":
			if r.URL.Query().Get("revision") != "commit-1" || r.URL.Query().Get("path") != name {
				t.Errorf("lost file locator: %s", r.URL.String())
				w.WriteHeader(400)
				return
			}
			fileRequests[ns]++
			if ns == repository.ModelScope {
				w.Header().Set("Trailer", "X-Dingo-Cache-Complete")
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(ns+" bytes")-1, len(ns+" bytes")))
				w.WriteHeader(206)
			}
			fmt.Fprint(w, ns+" bytes")
			if ns == repository.ModelScope {
				w.Header().Set("X-Dingo-Cache-Complete", "true")
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	config.SysConfig.Server.Host = host
	config.SysConfig.Server.Port, _ = strconv.Atoi(port)
	for _, ns := range []string{"alice", "用户#50%", repository.ModelScope} {
		key := repository.RepoKey{Namespace: ns, RepoType: "models", Repo: repo}
		job := &query.CreateCacheJobReq{Namespace: ns, Datatype: "models", Repo: repo}
		base := CacheTask{Ctx: context.Background(), Job: job}
		mount := MountCacheTask{CacheTask: base, Authorization: "Bearer provider"}
		if err := mount.mountViaRepositoryAPI(key, "main"); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(config.SysConfig.Cache.MountModelDir, "models", ns, filepath.FromSlash(repo), filepath.FromSlash(name)))
		if err != nil || string(content) != ns+" bytes" {
			t.Fatalf("mount %s: %q, %v", ns, content, err)
		}
		meta, err := ReadRepositoryMetadata(context.Background(), key, "main", "Bearer provider")
		if err != nil {
			t.Fatal(err)
		}
		preheat := PreheatCacheTask{CacheTask: base, Sha: meta, Authorization: "Bearer provider"}
		if err := preheat.preheatProcess(key.ID()); err != nil {
			t.Fatal(err)
		}
		if preheat.stockLen.Load() != uint64(len(ns+" bytes")) || fileRequests[ns] != 2 {
			t.Fatalf("preheat did not read canonical bytes for %s", ns)
		}
	}
}

func TestCanonicalTaskRequestsRefuseRedirectWithoutServiceToken(t *testing.T) {
	previous := config.SysConfig
	t.Cleanup(func() { config.SysConfig = previous })
	config.SysConfig = &config.Config{Server: config.ServerConfig{}}
	hits := 0
	untrusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(500) }))
	defer untrusted.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, untrusted.URL, 302) }))
	defer server.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	config.SysConfig.Server.Host = host
	config.SysConfig.Server.Port, _ = strconv.Atoi(port)
	key := repository.RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/model"}
	if _, err := ReadRepositoryMetadata(context.Background(), key, "main", ""); err == nil {
		t.Fatal("redirect accepted")
	}
	if hits != 0 {
		t.Fatal("machine request followed redirect")
	}
}
