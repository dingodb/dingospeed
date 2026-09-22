package service

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dingospeed/internal/data"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func msConfig(t *testing.T, endpoint string) {
	t.Helper()
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{
		Server:     config.ServerConfig{Repos: t.TempDir(), Online: true},
		Download:   config.Download{BlockSize: 1024, RespChanSize: 32, RespChunkSize: 512, RemoteFileRangeSize: 4096, RemoteFileBufferSize: 4096, GoroutineMaxNumPerFile: 4, ReqTimeout: 5},
		Modelscope: config.Modelscope{OfficialBaseURL: endpoint, MaxRetry: 1},
		Retry:      config.Retry{Attempts: 1},
	}
	data.NewBaseData()
}

func msEngine() *echo.Echo {
	s := NewModelscopeService()
	e := echo.New()
	e.Add(http.MethodGet, "/api/v1/models/:owner/:repo/repo", func(c echo.Context) error {
		return s.HandleFileDownload(c, c.Param("owner"), c.Param("repo"), "models")
	})
	e.Add(http.MethodHead, "/api/v1/models/:owner/:repo/repo", func(c echo.Context) error {
		return s.HandleFileDownload(c, c.Param("owner"), c.Param("repo"), "models")
	})
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		e.Add(method, "/file", func(c echo.Context) error {
			return s.RepositoryFile(c, repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: c.QueryParam("repo")}, c.QueryParam("revision"), c.QueryParam("path"))
		})
	}
	return e
}

func msRequest(e *echo.Echo, method, path, rng string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func msPayload(t *testing.T, key repository.RepoKey, oid string) ([]byte, *downloader.DingCacheHeader) {
	t.Helper()
	f, err := os.Open(key.Blob(config.SysConfig.Repos(), oid))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := &downloader.DingCacheHeader{}
	if err = h.Read(f); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b, h
}

func TestModelScopeSharedSingleFile(t *testing.T) {
	content := bytes.Repeat([]byte("scope-0123456789!"), 1024)
	var current atomic.Int64
	var bodies atomic.Int64
	var denied atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Dingo-Service-Token") != "" || r.Header.Get("X-Dingo-Ensure-Cache") != "" {
			t.Error("internal credential/control header leaked")
		}
		if denied.Load() {
			w.WriteHeader(403)
			return
		}
		data := content
		if current.Load() == 1 {
			data = bytes.Repeat([]byte("changed-7654321!!"), 1024)
		}
		file := r.URL.Query().Get("FilePath")
		revision := fmt.Sprintf("commit-%d", current.Load())
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			files := []map[string]any{}
			for _, name := range []string{"nested/weights.bin", "copy.bin", "sha1.txt", "empty.txt"} {
				b := data
				if name == "empty.txt" {
					b = nil
				}
				hash := sha256.Sum256(b)
				entry := map[string]any{"Type": "blob", "Path": name, "Revision": revision, "Size": len(b), "Sha256": hex.EncodeToString(hash[:])}
				if name == "sha1.txt" {
					delete(entry, "Sha256")
					h := sha1.Sum(b)
					entry["Sha1"] = hex.EncodeToString(h[:])
				}
				files = append(files, entry)
			}
			json.NewEncoder(w).Encode(map[string]any{"Code": 200, "Data": map[string]any{"Files": files}})
			return
		}
		if r.URL.Query().Get("Revision") != revision {
			t.Errorf("download was not pinned: %s", r.URL.RawQuery)
		}
		bodies.Add(1)
		if file == "empty.txt" {
			data = nil
		}
		start, end, err := util.FileRange(r.Header.Get("Range"), int64(len(data)))
		if err != nil {
			w.WriteHeader(416)
			return
		}
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(data)))
		}
		// Match the real provider's 200 + Content-Range response.
		w.Header().Set("Content-Length", fmt.Sprint(end-start))
		w.Write(data[start:end])
	}))
	defer upstream.Close()
	msConfig(t, upstream.URL)
	// LocalOnly must suppress ALL cluster calls even in a cluster-configured Speed.
	config.SysConfig.SetSchedulerModel("cluster")
	e := msEngine()
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "Qwen/demo"}
	native := "/api/v1/models/Qwen/demo/repo?Revision=master&FilePath=nested%2Fweights.bin"
	canonical := "/file?repo=Qwen%2Fdemo&revision=master&path=nested%2Fweights.bin"
	for _, tc := range []struct {
		method, path, rng string
		code              int
		want              []byte
	}{
		{"HEAD", native, "", 200, nil},
		{"GET", native, "bytes=0-2047", 206, content[:2048]},
		{"GET", canonical, "", 200, content},
	} {
		r := msRequest(e, tc.method, tc.path, tc.rng)
		if r.Code != tc.code || !bytes.Equal(r.Body.Bytes(), tc.want) {
			t.Fatalf("%s %s: %d %q", tc.method, tc.rng, r.Code, r.Body.Bytes()[:min(150, r.Body.Len())])
		}
	}
	hash := sha256.Sum256(content)
	oid := hex.EncodeToString(hash[:])
	b, h := msPayload(t, key, oid)
	if !bytes.Equal(b, content) || int(h.FileSize) != len(content) {
		t.Fatal("wrong dingcache payload")
	}
	for i := uint64(0); i < h.BlockNumber; i++ {
		set, _ := h.BlockMask.Test(i)
		if !set {
			t.Fatalf("missing block %d", i)
		}
	}
	before := bodies.Load()
	for _, rng := range []string{"", "bytes=0-0", "bytes=1024-", "bytes=-19"} {
		start, end, _ := util.FileRange(rng, int64(len(content)))
		r := msRequest(msEngine(), "GET", canonical, rng) // Recreate service and reopen the cached container.
		if !bytes.Equal(r.Body.Bytes(), content[start:end]) {
			t.Fatalf("cache range %s mismatch", rng)
		}
	}
	if bodies.Load() != before {
		t.Fatal("warm read fetched upstream body")
	}
	copyURL := "/file?repo=Qwen%2Fdemo&revision=master&path=copy.bin"
	if r := msRequest(e, "GET", copyURL, ""); !bytes.Equal(r.Body.Bytes(), content) {
		t.Fatal("shared content failed")
	}
	if bodies.Load() != before {
		t.Fatal("same OID different path did not reuse blob")
	}
	for _, path := range []string{"copy.bin", "nested/weights.bin"} {
		if _, err := os.Stat(key.Resolve(config.SysConfig.Repos(), "commit-0", path)); err != nil {
			t.Fatal(err)
		}
	}
	for _, rng := range []string{"bytes=999999-", "bytes=8-2", "bytes=1-2,4-5"} {
		if r := msRequest(e, "GET", canonical, rng); r.Code != 416 {
			t.Fatalf("bad range %s returned %d", rng, r.Code)
		}
	}
	if r := msRequest(e, "GET", "/file?repo=Qwen%2Fdemo&revision=master&path=sha1.txt", ""); r.Code != 200 || !bytes.Equal(r.Body.Bytes(), content) {
		t.Fatal("SHA1-only file rejected")
	}
	if r := msRequest(e, "GET", "/file?repo=Qwen%2Fdemo&revision=master&path=empty.txt", ""); r.Code != 200 || r.Body.Len() != 0 {
		t.Fatal("zero-byte file rejected")
	}
	denied.Store(true)
	if r := msRequest(e, "GET", canonical, ""); r.Code != 403 {
		t.Fatalf("cached bytes bypassed auth: %d", r.Code)
	}
	denied.Store(false)
	current.Store(1)
	if r := msRequest(e, "GET", canonical, ""); r.Code != 200 || bytes.Equal(r.Body.Bytes(), content) {
		t.Fatal("stale branch content")
	}
	old, _ := msPayload(t, key, oid)
	if !bytes.Equal(old, content) {
		t.Fatal("branch update overwrote old blob")
	}
}

func TestModelScopeStreamingAndCacheCompletion(t *testing.T) {
	content := bytes.Repeat([]byte("streaming-content"), 1024)
	hash := sha256.Sum256(content)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			fmt.Fprintf(w, `{"Code":200,"Data":{"Files":[{"Type":"blob","Path":"file.bin","Revision":"pinned","Sha256":"%x","Size":%d}]}}`, hash, len(content))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.Write(content[:1024])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Write(content[1024:])
	}))
	defer upstream.Close()
	msConfig(t, upstream.URL)
	config.SysConfig.Download.RemoteFileRangeSize = 0
	speed := httptest.NewServer(msEngine())
	defer speed.Close()
	req, _ := http.NewRequest("GET", speed.URL+"/file?repo=Qwen%2Fdemo&revision=master&path=file.bin", nil)
	req.Header.Set("X-Dingo-Ensure-Cache", "1")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 512)
	if _, err = io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, content[:512]) {
		t.Fatal("did not stream first bytes")
	}
	once.Do(func() { close(release) })
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(first, rest...), content) || resp.Trailer.Get("X-Dingo-Cache-Complete") != "true" {
		t.Fatalf("cache completion missing: %v", resp.Trailer)
	}
}

func TestModelScopeLiveSingleFile(t *testing.T) {
	if os.Getenv("DINGO_TEST_MODELSCOPE_LIVE") != "1" {
		t.Skip("opt-in real provider test")
	}
	msConfig(t, "https://modelscope.cn")
	config.SysConfig.Download.BlockSize = 64 * 1024
	config.SysConfig.Download.RemoteFileRangeSize = 256 * 1024
	config.SysConfig.Download.RespChunkSize = 32 * 1024
	config.SysConfig.Download.RemoteFileBufferSize = 256 * 1024
	e := msEngine()
	for _, file := range []string{"config.json", "merges.txt"} {
		target := "/file?" + url.Values{"repo": {"Qwen/Qwen2.5-0.5B-Instruct"}, "revision": {"master"}, "path": {file}}.Encode()
		r := msRequest(e, "GET", target, "")
		if r.Code != 200 {
			t.Fatalf("live %s: %d %s", file, r.Code, r.Body.String())
		}
		hash := sha256.Sum256(r.Body.Bytes())
		oid := hex.EncodeToString(hash[:])
		key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "Qwen/Qwen2.5-0.5B-Instruct"}
		payload, h := msPayload(t, key, oid)
		if !bytes.Equal(payload, r.Body.Bytes()) {
			t.Fatal("live payload mismatch")
		}
		warm := msRequest(msEngine(), "GET", target, "bytes=1-31")
		if warm.Code != 206 || !bytes.Equal(warm.Body.Bytes(), payload[1:32]) {
			t.Fatalf("live warm range mismatch: %d", warm.Code)
		}
		t.Logf("live file=%s commit=%s size=%d sha256=%s blocks=%d", file, r.Header().Get("X-Repo-Commit"), len(payload), oid, h.BlockNumber)
	}
}

func TestModelScopeLiveSDK(t *testing.T) {
	sdkPath := os.Getenv("DINGO_TEST_MODELSCOPE_SDK_PATH")
	if os.Getenv("DINGO_TEST_MODELSCOPE_LIVE") != "1" || sdkPath == "" {
		t.Skip("opt-in installed SDK test")
	}
	msConfig(t, "https://modelscope.cn")
	config.SysConfig.Download.BlockSize = 64 * 1024
	config.SysConfig.Download.RemoteFileRangeSize = 256 * 1024
	config.SysConfig.Download.RespChunkSize = 32 * 1024
	config.SysConfig.Download.RemoteFileBufferSize = 256 * 1024
	speed := httptest.NewServer(msEngine())
	defer speed.Close()
	script := `import sys,hashlib,json
sys.path.insert(0,sys.argv[1])
from modelscope_hub import HubApi, __version__
api=HubApi(endpoint=sys.argv[2],token='')
expected={'config.json':'18e18afcaccafade98daf13a54092927904649e1dd4eba8299ab717d5d94ff45','merges.txt':'599bab54075088774b1733fde865d5bd747cbcc7a547c5bc12610e874e26f5e3'}
for attempt in range(2):
 for name,sha in expected.items():
  p=api.download_file('Qwen/Qwen2.5-0.5B-Instruct',repo_type='model',file_path=name,revision='master',cache_dir=sys.argv[3],local_dir=sys.argv[3],force=True)
  b=p.read_bytes();actual=hashlib.sha256(b).hexdigest()
  assert actual==sha,(name,actual)
  print(json.dumps({'sdk':__version__,'attempt':attempt,'file':name,'size':len(b),'sha256':actual}))
`
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python", "-c", script, sdkPath, speed.URL, t.TempDir())
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatal(err)
	}
}

func TestModelScopeFailureDoesNotCompleteCache(t *testing.T) {
	for _, mode := range []string{"truncated", "wrong-range", "disk-write"} {
		t.Run(mode, func(t *testing.T) {
			content := bytes.Repeat([]byte("failure-test-1234"), 1024)
			hash := sha256.Sum256(content)
			oid := hex.EncodeToString(hash[:])
			var failing atomic.Bool
			failing.Store(true)
			key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "Qwen/demo"}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/repo/files") {
					fmt.Fprintf(w, `{"Code":200,"Data":{"Files":[{"Type":"blob","Path":"file.bin","Revision":"pinned","Sha256":"%s","Size":%d}]}}`, oid, len(content))
					return
				}
				start, end, _ := util.FileRange(r.Header.Get("Range"), int64(len(content)))
				if failing.Load() && mode == "disk-write" {
					path := key.Blob(config.SysConfig.Repos(), oid)
					if err := os.Remove(path); err != nil {
						t.Error(err)
					}
					if err := os.Mkdir(path, 0755); err != nil {
						t.Error(err)
					}
				}
				if failing.Load() && mode == "wrong-range" {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 5-%d/%d", end-1, len(content)))
				} else if r.Header.Get("Range") != "" {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(content)))
				}
				w.Header().Set("Content-Length", fmt.Sprint(end-start))
				if failing.Load() && mode == "truncated" {
					w.Write(content[start:min(end, start+1024)])
					return
				}
				w.Write(content[start:end])
			}))
			defer upstream.Close()
			msConfig(t, upstream.URL)
			config.SysConfig.Download.RemoteFileRangeSize = 0
			speed := httptest.NewServer(msEngine())
			defer speed.Close()
			request := func() ([]byte, string) {
				req, _ := http.NewRequest("GET", speed.URL+"/file?repo=Qwen%2Fdemo&revision=master&path=file.bin", nil)
				req.Header.Set("X-Dingo-Ensure-Cache", "1")
				resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return b, resp.Trailer.Get("X-Dingo-Cache-Complete")
			}
			body, complete := request()
			if complete == "true" {
				t.Fatalf("%s falsely completed cache", mode)
			}
			if mode == "disk-write" {
				if !bytes.Equal(body, content) {
					t.Fatal("cache failure interrupted otherwise valid upstream bytes")
				}
				if err := os.Remove(key.Blob(config.SysConfig.Repos(), oid)); err != nil {
					t.Fatal(err)
				} // Empty injected directory only.
			}
			failing.Store(false)
			body, complete = request()
			if complete != "true" || !bytes.Equal(body, content) {
				t.Fatalf("retry did not recover: complete=%s size=%d", complete, len(body))
			}
			payload, _ := msPayload(t, key, oid)
			if !bytes.Equal(payload, content) {
				t.Fatal("retry cache corrupted")
			}
		})
	}
}

func TestModelScopeConcurrentReaders(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	restore := zap.ReplaceGlobals(zap.New(core))
	defer restore()
	defer func() {
		if t.Failed() {
			for _, entry := range logs.All() {
				t.Log(entry.Message)
			}
		}
	}()
	content := bytes.Repeat([]byte("concurrent-bytes"), 1024)
	hash := sha256.Sum256(content)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			fmt.Fprintf(w, `{"Code":200,"Data":{"Files":[{"Type":"blob","Path":"file.bin","Revision":"pinned","Sha256":"%x","Size":%d}]}}`, hash, len(content))
			return
		}
		start, end, _ := util.FileRange(r.Header.Get("Range"), int64(len(content)))
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(content)))
		}
		w.Header().Set("Content-Length", fmt.Sprint(end-start))
		w.Write(content[start:end])
	}))
	defer upstream.Close()
	msConfig(t, upstream.URL)
	e := msEngine()
	handler := e.HTTPErrorHandler
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		if !c.Response().Committed {
			t.Logf("concurrent handler error: %v", err)
		}
		handler(err, c)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := msRequest(e, "GET", "/file?repo=Qwen%2Fdemo&revision=master&path=file.bin", "")
			if r.Code != 200 || !bytes.Equal(r.Body.Bytes(), content) {
				t.Errorf("concurrent response mismatch: %d bytes=%d body=%q", r.Code, r.Body.Len(), r.Body.String())
			}
		}()
	}
	wg.Wait()
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "Qwen/demo"}
	payload, _ := msPayload(t, key, hex.EncodeToString(hash[:]))
	if !bytes.Equal(payload, content) {
		t.Fatal("concurrent cache corrupted")
	}
}

func TestModelScopeProcessReopen(t *testing.T) {
	if root := os.Getenv("DINGO_MS_REOPEN_ROOT"); root != "" {
		msConfig(t, os.Getenv("DINGO_MS_REOPEN_UPSTREAM"))
		config.SysConfig.Server.Repos = root
		r := msRequest(msEngine(), "GET", "/file?repo=Qwen%2Fdemo&revision=master&path=file.bin", "")
		if r.Code != 200 || !bytes.Equal(r.Body.Bytes(), bytes.Repeat([]byte("persisted-bytes!"), 1024)) {
			t.Fatal("new process did not read cached payload")
		}
		return
	}
	content := bytes.Repeat([]byte("persisted-bytes!"), 1024)
	hash := sha256.Sum256(content)
	var rejectBody atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			fmt.Fprintf(w, `{"Code":200,"Data":{"Files":[{"Type":"blob","Path":"file.bin","Revision":"pinned","Sha256":"%x","Size":%d}]}}`, hash, len(content))
			return
		}
		if rejectBody.Load() {
			t.Error("restarted process refetched body")
			w.WriteHeader(503)
			return
		}
		start, end, _ := util.FileRange(r.Header.Get("Range"), int64(len(content)))
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(content)))
		}
		w.Header().Set("Content-Length", fmt.Sprint(end-start))
		w.Write(content[start:end])
	}))
	defer upstream.Close()
	msConfig(t, upstream.URL)
	r := msRequest(msEngine(), "GET", "/file?repo=Qwen%2Fdemo&revision=master&path=file.bin", "")
	if r.Code != 200 || !bytes.Equal(r.Body.Bytes(), content) {
		t.Fatal("cold fill failed")
	}
	rejectBody.Store(true)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestModelScopeProcessReopen$", "-test.v")
	cmd.Env = append(os.Environ(), "DINGO_MS_REOPEN_ROOT="+config.SysConfig.Repos(), "DINGO_MS_REOPEN_UPSTREAM="+upstream.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
}

func TestModelScopeCancelledTransferResumes(t *testing.T) {
	content := bytes.Repeat([]byte("cancel-and-retry"), 1024)
	hash := sha256.Sum256(content)
	var first atomic.Bool
	first.Store(true)
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			fmt.Fprintf(w, `{"Code":200,"Data":{"Files":[{"Type":"blob","Path":"file.bin","Revision":"pinned","Sha256":"%x","Size":%d}]}}`, hash, len(content))
			return
		}
		start, end, _ := util.FileRange(r.Header.Get("Range"), int64(len(content)))
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(content)))
		}
		w.Header().Set("Content-Length", fmt.Sprint(end-start))
		if first.CompareAndSwap(true, false) {
			defer close(upstreamDone)
			w.Write(content[:2048])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Write(content[start:end])
	}))
	defer upstream.Close()
	msConfig(t, upstream.URL)
	config.SysConfig.Download.RemoteFileRangeSize = 0
	speed := httptest.NewServer(msEngine())
	defer speed.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", speed.URL+"/file?repo=Qwen%2Fdemo&revision=master&path=file.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	if _, err = io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream was not cancelled")
	}
	r := msRequest(msEngine(), "GET", "/file?repo=Qwen%2Fdemo&revision=master&path=file.bin", "")
	if r.Code != 200 || !bytes.Equal(r.Body.Bytes(), content) {
		t.Fatalf("retry after cancellation failed: %d %d", r.Code, r.Body.Len())
	}
	key := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "Qwen/demo"}
	payload, _ := msPayload(t, key, hex.EncodeToString(hash[:]))
	if !bytes.Equal(payload, content) {
		t.Fatal("cancel/retry cache mismatch")
	}
}
