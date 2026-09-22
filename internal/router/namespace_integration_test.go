package router

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/downloader"
	"dingospeed/internal/handler"
	"dingospeed/internal/service"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"github.com/labstack/echo/v4"
)

func namespaceEngines(t *testing.T) (*echo.Echo, *echo.Echo) {
	t.Helper()
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir()}, Upload: config.Upload{Namespace: "dingo-local", ConcurrentLimit: 4}, Download: config.Download{GoroutineMaxNumPerFile: 4, BlockSize: 1024, RespChanSize: 32, RespChunkSize: 1024, RemoteFileRangeSize: 4096, RemoteFileBufferSize: 4096, ReqTimeout: 5}, Retry: config.Retry{Attempts: 1}}
	base := data.NewBaseData()
	lock := dao.NewLockDao(base)
	file := dao.NewFileDao(dao.NewDownloaderDao(nil), base, lock)
	meta := handler.NewMetaHandler(service.NewMetaService(file, dao.NewMetaDao(file, lock, base)))
	read := echo.New()
	NewHttpRouter(read, handler.NewFileHandler(service.NewFileService(file), nil, nil), meta, handler.NewSysHandler(nil), handler.NewCacheJobHandler(nil), handler.NewModelscopeHandler(service.NewModelscopeService()))
	upload := echo.New()
	NewUploadRouter(UploadEcho{upload}, handler.NewUploadHandler(service.NewUploadService(dao.NewUploadDao(file, lock))), handler.NewCacheAdminHandler(service.NewCacheAdminService(dao.NewCacheAdminDao(file))))
	return read, upload
}
func namespaceRequest(e *echo.Echo, method, target string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}
func requireStatus(t *testing.T, r *httptest.ResponseRecorder, status int) {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status %d want %d: %s", r.Code, status, r.Body.String())
	}
}
func digest(content []byte) string { s := sha256.Sum256(content); return hex.EncodeToString(s[:]) }
func locator(repo, revision, path string) string {
	q := url.Values{"repo": {repo}, "revision": {revision}}
	if path != "" {
		q.Set("path", path)
	}
	return q.Encode()
}

func TestNamespacesUploadPublishReadAndArchive(t *testing.T) {
	read, upload := namespaceEngines(t)
	const repo = "team/resolve/model-a"
	files := []string{"weights/nested/model.bin", "notes/50%.txt", "notes/literal%2F.txt"}
	for _, ns := range []string{"alice", "bob", "datacanvas", "用户#50%"} {
		routeNS := url.PathEscape(ns)
		items := []map[string]interface{}{}
		for _, file := range files {
			content := []byte(ns + ":" + file)
			q := locator(repo, "main", file) + "&size=" + fmt.Sprint(len(content)) + "&sha256=" + digest(content) + "&defer=true"
			r := namespaceRequest(upload, "POST", "/api/uploads/models/"+routeNS+"?"+q, content)
			requireStatus(t, r, 201)
			var got map[string]interface{}
			json.Unmarshal(r.Body.Bytes(), &got)
			if got["namespace"] != ns || got["repo"] != repo || got["commit"] != "" {
				t.Fatalf("staging identity wrong: %s", r.Body.String())
			}
			r = namespaceRequest(upload, "GET", "/api/upload-progress/models/"+routeNS+"?"+locator(repo, "main", file)+"&sha256="+digest(content), nil)
			requireStatus(t, r, 200)
			json.Unmarshal(r.Body.Bytes(), &got)
			if got["blobComplete"] != true || got["effective"] != false {
				t.Fatalf("staging progress wrong: %s", r.Body.String())
			}
			items = append(items, map[string]interface{}{"path": file, "size": len(content), "sha256": digest(content)})
		}
		body, _ := json.Marshal(map[string]interface{}{"files": items})
		r := namespaceRequest(upload, "POST", "/api/publish/models/"+routeNS+"?"+locator(repo, "main", ""), body)
		requireStatus(t, r, 201)
		r = namespaceRequest(read, "GET", "/api/repositories/models/"+routeNS+"/snapshot?"+locator(repo, "main", ""), nil)
		requireStatus(t, r, 200)
		var snap map[string]interface{}
		json.Unmarshal(r.Body.Bytes(), &snap)
		if snap["namespace"] != ns || snap["repo"] != repo || len(snap["files"].([]interface{})) != len(files) {
			t.Fatalf("snapshot identity wrong: %s", r.Body.String())
		}
		for _, file := range files {
			r = namespaceRequest(read, "GET", "/api/repositories/models/"+routeNS+"/file?"+locator(repo, "main", file), nil)
			requireStatus(t, r, 200)
			if r.Body.String() != ns+":"+file {
				t.Fatalf("cross-namespace file: %s", r.Body.String())
			}
			requireStatus(t, namespaceRequest(read, "HEAD", "/api/repositories/models/"+routeNS+"/file?"+locator(repo, "main", file), nil), 200)
		}
		r = namespaceRequest(read, "GET", "/api/repositories/models/"+routeNS+"/archive?"+locator(repo, "main", ""), nil)
		requireStatus(t, r, 200)
		archive, err := zip.NewReader(bytes.NewReader(r.Body.Bytes()), int64(r.Body.Len()))
		if err != nil {
			t.Fatal(err)
		}
		if len(archive.File) != len(files) {
			t.Fatalf("archive file count %d", len(archive.File))
		}
		for _, entry := range archive.File {
			f, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(f)
			f.Close()
			if !strings.HasPrefix(string(b), ns+":") {
				t.Fatal("archive crossed namespaces")
			}
		}
		r = namespaceRequest(read, "GET", "/api/repositories/models/"+routeNS+"/directories", nil)
		requireStatus(t, r, 200)
		if !strings.Contains(r.Body.String(), `"repo":"`+repo+`"`) {
			t.Fatal(r.Body.String())
		}
		r = namespaceRequest(read, "GET", "/api/repositories/models/"+routeNS+"/revisions?repo="+url.QueryEscape(repo), nil)
		requireStatus(t, r, 200)
		if !strings.Contains(r.Body.String(), `"name":"main"`) {
			t.Fatal(r.Body.String())
		}
	}
	// Publish-tree edits only alice. The other namespace and the immutable base
	// commit remain independently readable while the new branch head changes.
	base := namespaceRequest(read, "GET", "/api/repositories/models/alice/snapshot?"+locator(repo, "main", ""), nil)
	requireStatus(t, base, 200)
	var snap struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(base.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	changed := []byte("alice edited content")
	newPath := "edits/repository.json"
	staged := namespaceRequest(upload, "POST", "/api/uploads/models/alice?"+locator(repo, "main", newPath)+"&size="+fmt.Sprint(len(changed))+"&sha256="+digest(changed)+"&defer=true", changed)
	requireStatus(t, staged, 201)
	body, _ := json.Marshal(map[string]interface{}{"baseCommit": snap.Commit, "files": []map[string]interface{}{{"path": newPath, "size": len(changed), "sha256": digest(changed)}}})
	updated := namespaceRequest(upload, "POST", "/api/publish-tree/models/alice?"+locator(repo, "main", ""), body)
	requireStatus(t, updated, 201)
	if !strings.Contains(updated.Body.String(), `"removed":3`) {
		t.Fatal(updated.Body.String())
	}
	requireStatus(t, namespaceRequest(read, "GET", "/api/repositories/models/alice/file?"+locator(repo, "main", files[0]), nil), 404)
	requireStatus(t, namespaceRequest(read, "GET", "/api/repositories/models/alice/file?"+locator(repo, snap.Commit, files[0]), nil), 200)
	untouched := namespaceRequest(read, "GET", "/api/repositories/models/bob/file?"+locator(repo, "main", files[0]), nil)
	requireStatus(t, untouched, 200)
	if untouched.Body.String() != "bob:"+files[0] {
		t.Fatal("tree publish changed bob")
	}
	// A different edit based on the superseded head must fail its optimistic lock.
	body, _ = json.Marshal(map[string]interface{}{"baseCommit": snap.Commit, "files": []interface{}{}})
	requireStatus(t, namespaceRequest(upload, "POST", "/api/publish-tree/models/alice?"+locator(repo, "main", ""), body), 409)

}

func TestRepositoryAPIRejectsBadLocatorsWithoutServiceToken(t *testing.T) {
	read, upload := namespaceEngines(t)
	for _, q := range []string{"repo=a&repo=b&revision=main&path=x", "repo=a&revision=main&revision=v1&path=x", "repo=a&revision=main&path=x&path=y", "repo=a//b&revision=main&path=x", "repo=a/../b&revision=main&path=x", "repo=a&revision=../main&path=x", "repo=a&revision=main&path=../secret", "repo=a&revision=main&path=x&namespace=bob", "repo=a&revision=main&path=x%ZZ"} {
		requireStatus(t, namespaceRequest(upload, "POST", "/api/uploads/models/alice?"+q, []byte("x")), 400)
	}
	for _, ns := range []string{"huggingface", "modelscope"} {
		requireStatus(t, namespaceRequest(upload, "POST", "/api/uploads/models/"+ns+"?repo=Qwen/Qwen3&revision=main&path=a&size=1&sha256="+digest([]byte("x")), []byte("x")), 400)
	}
	requireStatus(t, namespaceRequest(read, "GET", "/api/repositories/models/unknown/snapshot?repo=team/model&revision=main", nil), 404)
	requireStatus(t, namespaceRequest(read, "GET", "/api/local-repositories/models/dingo-local/demo/revisions/main", nil), 404)
	requireStatus(t, namespaceRequest(upload, "GET", "/api/cache/summary", nil), 200)
}

func TestHFAndModelScopeProtocolIsolationWithMockUpstreams(t *testing.T) {
	read, _ := namespaceEngines(t)
	hfContent := []byte("huggingface file content")
	msContent := []byte("modelscope original bytes")
	var denied atomic.Bool
	var hfRequests, msRequests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Dingo-Service-Token") != "" {
			t.Error("machine token leaked upstream")
		}
		if strings.Contains(r.URL.Path, "huggingface/") || strings.Contains(r.URL.Path, "modelscope/") {
			t.Error("platform namespace leaked upstream")
		}
		if denied.Load() {
			w.WriteHeader(403)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/prefix")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(path, "/api/v1/") {
			msRequests.Add(1)
			if path == "/api/v1/models/Qwen/Qwen3/repo" {
				if r.URL.Query().Get("FilePath") != "nested/weights.bin" {
					t.Error("MS file path lost")
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				if r.Header.Get("Range") == "bytes=0-0" {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(msContent)))
					w.WriteHeader(206)
					w.Write(msContent[:1])
					return
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(msContent)))
				w.Write(msContent)
				return
			}
			if path == "/api/v1/models/Qwen/Qwen3/revisions" {
				fmt.Fprint(w, `{"Code":200,"Data":{"RevisionMap":{"Branches":[{"Revision":"master","CommitId":"ms-commit-one"}],"Tags":[]}}}`)
				return
			}
			if path == "/api/v1/models/Qwen/Qwen3/repo/files" {
				fmt.Fprintf(w, `{"Code":200,"Data":{"Files":[{"Type":"blob","Path":"nested/weights.bin","Name":"weights.bin","Size":%d,"Sha256":%q}]}}`, len(msContent), digest(msContent))
				return
			}
			fmt.Fprint(w, `{"Code":200,"Data":{"Id":"Qwen/Qwen3"}}`)
			return
		}
		hfRequests.Add(1)
		switch {
		case strings.HasPrefix(path, "/api/models/Qwen/Qwen3/revision/"):
			fmt.Fprintf(w, `{"id":"Qwen/Qwen3","sha":"commit-one","siblings":[{"rfilename":"nested/weights.bin"}],"usedStorage":%d}`, len(hfContent))
		case path == "/api/models/Qwen/Qwen3/paths-info/commit-one":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "nested/weights.bin") {
				t.Error("HF nested file path lost")
			}
			fmt.Fprintf(w, `[{"type":"file","oid":%q,"size":%d,"path":"nested/weights.bin","lfs":{"oid":%q,"size":%d}}]`, digest(hfContent), len(hfContent), digest(hfContent), len(hfContent))
		case path == "/Qwen/Qwen3/resolve/commit-one/nested/weights.bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprint(len(hfContent)))
			w.Write(hfContent)
		case path == "/api/models/Qwen/Qwen3/tree/main":
			if r.URL.Query().Get("cursor") == "next" {
				fmt.Fprint(w, `[{"type":"file","path":"page-two.txt","size":1}]`)
				return
			}
			w.Header().Set("Link", `<http://`+r.Host+`/prefix/api/models/Qwen/Qwen3/tree/main?cursor=next>; rel="next"`)
			fmt.Fprint(w, `[{"type":"file","path":"nested/weights.bin","size":23}]`)
		case path == "/api/models/Qwen/Qwen3/refs":
			fmt.Fprint(w, `{"branches":[{"name":"main","targetCommit":"commit-one"}]}`)
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	if err := dao.RegisterHosted("models", "Qwen", "Qwen3"); err != nil {
		t.Fatal(err)
	}
	config.SysConfig.Server.Online = true
	config.SysConfig.Server.HfScheme = "http"
	config.SysConfig.Server.HfNetLoc = strings.TrimPrefix(upstream.URL, "http://") + "/prefix"
	config.SysConfig.Modelscope.OfficialBaseURL = upstream.URL + "/prefix"
	config.SysConfig.Modelscope.MaxRetry = 1
	config.SysConfig.Modelscope.ChunkSize = 1024
	originalRoot := config.SysConfig.Server.Repos
	for _, cold := range []struct{ method, operation string }{{"HEAD", "file"}, {"GET", "file"}, {"GET", "tree"}} {
		config.SysConfig.Server.Repos = t.TempDir()
		k := repository.RepoKey{Namespace: repository.HuggingFace, RepoType: "models", Repo: "Qwen/Qwen3"}
		if _, err := repository.Read(config.SysConfig.Repos(), k); !os.IsNotExist(err) {
			t.Fatal("cold test already registered")
		}
		target := "/api/repositories/models/huggingface/" + cold.operation + "?" + locator("Qwen/Qwen3", "main", "nested/weights.bin")
		if cold.operation == "tree" {
			target = "/api/repositories/models/huggingface/tree?" + locator("Qwen/Qwen3", "main", "")
		}
		response := namespaceRequest(read, cold.method, target, nil)
		requireStatus(t, response, 200)
		if cold.operation == "tree" && !strings.Contains(response.Body.String(), "page-two.txt") {
			t.Fatal("canonical tree lost pagination")
		}
		if cold.method == "GET" && cold.operation == "file" && !bytes.Equal(response.Body.Bytes(), hfContent) {
			t.Fatal("cold canonical HF file mismatch")
		}
		if _, err := repository.Read(config.SysConfig.Repos(), k); err != nil {
			t.Fatal("cold HF read did not register source", err)
		}
	}
	config.SysConfig.Server.Repos = originalRoot
	for _, path := range []string{"/api/models/Qwen/Qwen3/revision/main", "/api/models/Qwen/Qwen3/tree/main?recursive=true", "/api/models/Qwen/Qwen3/refs"} {
		requireStatus(t, namespaceRequest(read, "GET", "/huggingface"+path, nil), 200)
		requireStatus(t, namespaceRequest(read, "GET", path, nil), 200)
	}
	paged := namespaceRequest(read, "GET", "/huggingface/api/models/Qwen/Qwen3/tree/main?recursive=true", nil)
	requireStatus(t, paged, 200)
	if link := paged.Header().Get("Link"); !strings.Contains(link, "/huggingface/api/models/Qwen/Qwen3/tree/main?cursor=next") || strings.Contains(link, "/prefix/") {
		t.Fatalf("HF pagination lost explicit endpoint: %s", link)
	}
	r := namespaceRequest(read, "GET", "/huggingface/Qwen/Qwen3/resolve/main/nested/weights.bin", nil)
	requireStatus(t, r, 200)
	if !bytes.Equal(r.Body.Bytes(), hfContent) {
		t.Fatalf("HF payload mismatch: %q", r.Body.String())
	}
	plain := namespaceRequest(read, "GET", "/Qwen/Qwen3/resolve/main/nested/weights.bin", nil)
	requireStatus(t, plain, 200)
	if !bytes.Equal(plain.Body.Bytes(), hfContent) {
		t.Fatal("hosted namespace intercepted original upstream download")
	}
	for _, path := range []string{"/api/v1/models/Qwen/Qwen3", "/api/v1/models/Qwen/Qwen3/revisions", "/api/v1/models/Qwen/Qwen3/repo/files"} {
		requireStatus(t, namespaceRequest(read, "GET", "/modelscope"+path, nil), 200)
	}
	msURL := "/modelscope/api/v1/models/Qwen/Qwen3/repo?Revision=master&FilePath=nested%2Fweights.bin"
	r = namespaceRequest(read, "GET", msURL, nil)
	if r.Code != 200 && r.Code != 206 {
		t.Fatal(r.Body.String())
	}
	if !bytes.Equal(r.Body.Bytes(), msContent) {
		t.Fatalf("MS payload mismatch: %q", r.Body.String())
	}
	hf := repository.RepoKey{Namespace: repository.HuggingFace, RepoType: "models", Repo: "Qwen/Qwen3"}
	ms := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "Qwen/Qwen3"}
	for _, key := range []repository.RepoKey{hf, ms} {
		if _, err := repository.Read(config.SysConfig.Repos(), key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(hf.Blob(config.SysConfig.Repos(), digest(hfContent))); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(ms.Blob(config.SysConfig.Repos(), digest(msContent)))
	if err != nil {
		t.Fatal(err)
	}
	header := &downloader.DingCacheHeader{}
	if err = header.Read(fh); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(fh)
	fh.Close()
	if err != nil || !bytes.Equal(raw, msContent) {
		t.Fatalf("MS dingcache payload wrong: %v %q", err, raw)
	}
	if _, err = os.Stat(ms.Resolve(config.SysConfig.Repos(), "ms-commit-one", "nested/weights.bin")); err != nil {
		t.Fatal(err)
	}

	for _, op := range []string{"metadata", "tree", "files", "revisions"} {
		target := "/api/repositories/models/modelscope/" + op + "?repo=Qwen%2FQwen3&revision=master"
		canonical := namespaceRequest(read, "GET", target, nil)
		requireStatus(t, canonical, 200)
		if op == "metadata" && (!strings.Contains(canonical.Body.String(), `"rfilename":"nested/weights.bin"`) || !strings.Contains(canonical.Body.String(), `"sha":"ms-commit-one"`)) {
			t.Fatalf("ModelScope canonical metadata: %s", canonical.Body.String())
		}
		if op == "revisions" && !strings.Contains(canonical.Body.String(), `"name":"master"`) {
			t.Fatalf("ModelScope canonical revisions: %s", canonical.Body.String())
		}
	}
	canonicalFile := "/api/repositories/models/modelscope/file?" + locator("Qwen/Qwen3", "master", "nested/weights.bin")
	canonical := namespaceRequest(read, "GET", canonicalFile, nil)
	if canonical.Code != 200 && canonical.Code != 206 {
		t.Fatal(canonical.Body.String())
	}
	if !bytes.Equal(canonical.Body.Bytes(), msContent) {
		t.Fatalf("canonical ModelScope payload wrong: %q", canonical.Body.String())
	}
	requireStatus(t, namespaceRequest(read, "HEAD", canonicalFile, nil), 200)
	if hfRequests.Load() == 0 || msRequests.Load() == 0 {
		t.Fatal("missing upstream protocol requests")
	}
	denied.Store(true)
	for _, op := range []string{"metadata", "tree", "revisions"} {
		requireStatus(t, namespaceRequest(read, "GET", "/api/repositories/models/modelscope/"+op+"?repo=Qwen%2FQwen3&revision=master", nil), 403)
	}
	requireStatus(t, namespaceRequest(read, "GET", "/api/repositories/models/huggingface/tree?repo=Qwen%2FQwen3&revision=main", nil), 403)
	requireStatus(t, namespaceRequest(read, "GET", canonicalFile, nil), 403)
	requireStatus(t, namespaceRequest(read, "GET", "/huggingface/Qwen/Qwen3/resolve/main/nested/weights.bin", nil), 403)
	requireStatus(t, namespaceRequest(read, "GET", msURL, nil), 403)
}
