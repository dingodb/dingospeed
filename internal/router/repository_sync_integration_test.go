package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/handler"
	"dingospeed/internal/service"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"github.com/labstack/echo/v4"
)

// Each child is a real UploadRouter backed by its own Speed cache directory and
// process-global config. Only Scheduler discovery is supplied by the parent
// fixture; bytes are uploaded, served and published by the production handlers.
func TestRepositorySyncProcessHelper(t *testing.T) {
	root := os.Getenv("DINGO_SYNC_TEST_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: root}, Download: config.Download{BlockSize: 1024, RespChunkSize: 1024, RespChanSize: 4, GoroutineMaxNumPerFile: 2, RemoteFileRangeSize: 2048}, Upload: config.Upload{Namespace: repository.Local, ConcurrentLimit: 2}, Scheduler: config.Scheduler{Mode: "cluster", HTTPURL: os.Getenv("DINGO_SYNC_TEST_SCHEDULER")}}
	b := data.NewBaseData()
	lock := dao.NewLockDao(b)
	file := dao.NewFileDao(nil, b, lock)
	if origin := os.Getenv("DINGO_SYNC_BROWSER_ORIGIN"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil {
			t.Fatal(err)
		}
		config.SysConfig.Server.Online = true
		config.SysConfig.Server.HfScheme = "http"
		config.SysConfig.Server.HfNetLoc = parsed.Host
		config.SysConfig.Modelscope.OfficialBaseURL = origin
		config.SysConfig.Retry.Attempts = 1
		config.SysConfig.Download.RemoteFileBufferSize = 1 << 20
		file = dao.NewFileDao(dao.NewDownloaderDao(nil), b, lock)
	}
	e := echo.New()
	e.HideBanner = true
	var ranges atomic.Int64
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Header.Get("Range") != "" {
				ranges.Add(1)
			}
			return next(c)
		}
	})
	uploadRouter := NewUploadRouter(UploadEcho{e}, handler.NewUploadHandler(service.NewUploadService(dao.NewUploadDao(file, lock))), handler.NewCacheAdminHandler(service.NewCacheAdminService(dao.NewCacheAdminDao(file))))
	if os.Getenv("DINGO_SYNC_BROWSER_MODE") == "1" {
		config.SysConfig.Download.BlockSize = 1 << 20
		config.SysConfig.Download.RespChunkSize = 64 << 10
		config.SysConfig.Download.RemoteFileRangeSize = 2 << 20
		meta := handler.NewMetaHandler(service.NewMetaService(file, dao.NewMetaDao(file, lock, b)))
		NewHttpRouter(e, handler.NewFileHandler(service.NewFileService(file), nil, nil), meta, handler.NewSysHandler(nil), handler.NewCacheJobHandler(nil), handler.NewModelscopeHandler(service.NewModelscopeService()))
		e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				if strings.HasPrefix(c.Request().URL.Path, "/api/uploads/") || strings.HasPrefix(c.Request().URL.Path, "/api/upload-chunks/") {
					if delay := os.Getenv("DINGO_SYNC_BROWSER_SLOW_UPLOAD"); delay == "1" {
						c.Request().Body = &slowBrowserBody{ReadCloser: c.Request().Body, ctx: c.Request().Context()}
					}
				}
				if c.Request().Header.Get("Range") != "" {
					select {
					case <-c.Request().Context().Done():
						return c.Request().Context().Err()
					case <-time.After(400 * time.Millisecond):
					}
				}
				return next(c)
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go uploadRouter.RunSyncRecovery(ctx)
	}
	// Inspection uses the actual targeted snapshot reader, not a fake state.
	e.GET("/test/snapshot", func(c echo.Context) error {
		commit, files, err := dao.ReadInventorySnapshot(repository.RepoKey{Namespace: repository.Local, RepoType: "models", Repo: "demo"}, c.QueryParam("revision"))
		if err != nil {
			return c.NoContent(404)
		}
		return c.JSON(200, map[string]any{"commit": commit, "files": files})
	})
	e.GET("/test/ranges", func(c echo.Context) error {
		return c.JSON(200, ranges.Load())
	})
	srv := httptest.NewUnstartedServer(e)
	if bind := os.Getenv("DINGO_SYNC_TEST_BIND"); bind != "" {
		listener, err := net.Listen("tcp", bind)
		if err != nil {
			t.Fatal(err)
		}
		srv.Listener.Close()
		srv.Listener = listener
	}
	srv.Start()
	defer srv.Close()
	if err := os.WriteFile(filepath.Join(root, "ready"), []byte(srv.URL), 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func syncNodeProcess(t *testing.T, scheduler string) string {
	return syncNodeProcessRole(t, scheduler, "")
}

func syncNodeProcessRole(t *testing.T, scheduler, requestedRole string) string {
	t.Helper()
	root := t.TempDir()
	bind := ""
	if home := os.Getenv("DINGO_SYNC_BROWSER_PERSIST_ROOT"); home != "" {
		role := "source"
		bind = "127.0.0.1:57516"
		if scheduler != "" {
			role = "target"
			bind = "127.0.0.1:57518"
		}
		if requestedRole != "" {
			role = requestedRole
			switch role {
			case "source":
				bind = "127.0.0.1:57516"
			case "target":
				bind = "127.0.0.1:57518"
			case "target2":
				bind = "127.0.0.1:57519"
			}
		}
		root = filepath.Join(home, role)
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRepositorySyncProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "DINGO_SYNC_TEST_ROOT="+root, "DINGO_SYNC_TEST_BIND="+bind, "DINGO_SYNC_TEST_SCHEDULER="+scheduler, "DINGO_SYNC_KEY=")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(root, "process.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		stdin.Close()
		select {
		case err := <-done:
			if err != nil {
				b, _ := os.ReadFile(log.Name())
				t.Errorf("Speed subprocess failed: %v\n%s", err, b)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("Speed subprocess failed to exit")
		}
		log.Close()
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(root, "ready")); err == nil && bytes.HasPrefix(b, []byte("http://")) {
			return string(b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(log.Name())
	t.Fatalf("Speed subprocess did not start: %s", b)
	return ""
}

func syncHTTP(t *testing.T, method, target string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func TestRepositorySyncTwoSpeedProcesses(t *testing.T) {
	source := syncNodeProcess(t, "")
	payload := bytes.Repeat([]byte("actual cached body served by source Speed"), 1024)
	sum := sha256.Sum256(payload)
	sha := hex.EncodeToString(sum[:])
	uploadQuery := url.Values{"repo": {"demo"}, "revision": {"release"}, "path": {"model.bin"}, "sha256": {sha}, "size": {fmt.Sprint(len(payload))}}
	code, b := syncHTTP(t, "POST", source+"/api/uploads/models/dingo-local?"+uploadQuery.Encode(), payload)
	if code != 201 {
		t.Fatalf("source upload %d %s", code, b)
	}
	code, b = syncHTTP(t, "GET", source+"/api/repository-sync/models/dingo-local/content?"+uploadQuery.Encode(), nil)
	if code != 200 || !bytes.Equal(b, payload) {
		t.Fatalf("source cached payload mismatch: HTTP %d got %d bytes want %d", code, len(b), len(payload))
	}
	for _, start := range []int{0, 17, 2048, 4096} {
		req, _ := http.NewRequest("GET", source+"/api/repository-sync/models/dingo-local/content?"+uploadQuery.Encode(), nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+2047))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		part, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 206 || !bytes.Equal(part, payload[start:start+2048]) {
			t.Fatalf("source range %d differs: status=%d len=%d err=%v", start, resp.StatusCode, len(part), err)
		}
	}
	for _, invalid := range []string{"bytes=4096-2048", "bytes=0-1,8-9", "bytes=-100"} {
		req, _ := http.NewRequest("GET", source+"/api/repository-sync/models/dingo-local/content?"+uploadQuery.Encode(), nil)
		req.Header.Set("Range", invalid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		part, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 416 || len(part) != 0 || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes */%d", len(payload)) {
			t.Fatalf("invalid range accepted: %s, HTTP %d", invalid, resp.StatusCode)
		}
	}
	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/upload-inventory/sources" || r.Header.Get("Authorization") != "" || r.URL.Query().Get("sha256") != sha || r.URL.Query().Get("repo") != "demo" {
			http.Error(w, "bad exact lookup", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []service.SyncSource{{InstanceID: "source", ManagementURL: source}}})
	}))
	defer scheduler.Close()
	target := syncNodeProcess(t, scheduler.URL)
	request := service.SyncRequest{TaskID: "two-speed", Repo: "demo", Revision: "release", ExpectedAbsent: true, Files: []dao.LocalManifestFile{{Path: "model.bin", Sha256: sha, Size: int64(len(payload))}}}
	body, _ := json.Marshal(request)
	endpoint := target + "/api/repository-sync/models/dingo-local/tasks"
	if code, b = syncHTTP(t, "POST", endpoint, body); code != 202 {
		t.Fatalf("start %d %s", code, b)
	}
	deadline := time.Now().Add(10 * time.Second)
	var status service.SyncStatus
	for time.Now().Before(deadline) {
		code, b = syncHTTP(t, "GET", target+"/api/repository-sync/tasks/two-speed", nil)
		if code != 200 {
			t.Fatalf("status %d %s", code, b)
		}
		if err := json.Unmarshal(b, &status); err != nil {
			t.Fatal(err)
		}
		if status.State == "completed" {
			break
		}
		if status.State == "failed" || status.State == "conflict" || status.State == "result_unconfirmed" {
			t.Fatalf("task %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.State != "completed" {
		t.Fatalf("task did not finish: %+v", status)
	}
	code, b = syncHTTP(t, "GET", source+"/test/ranges", nil)
	var ranges int64
	// Four valid probes plus three rejected ranges precede the actual task.
	if code != 200 || json.Unmarshal(b, &ranges) != nil || ranges < 9 {
		t.Fatalf("multi-block sync did not issue multiple source ranges: HTTP %d %s", code, b)
	}
	code, b = syncHTTP(t, "GET", target+"/test/snapshot?revision=release", nil)
	if code != 200 {
		t.Fatalf("snapshot %d %s", code, b)
	}
	var snapshot struct {
		Commit string                  `json:"commit"`
		Files  []dao.LocalManifestFile `json:"files"`
	}
	if err := json.Unmarshal(b, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Commit != status.Commit || len(snapshot.Files) != 1 || snapshot.Files[0] != request.Files[0] {
		t.Fatalf("wrong actual target snapshot: %+v", snapshot)
	}
	contentQuery := url.Values{"repo": {"demo"}, "path": {"model.bin"}, "sha256": {sha}, "size": {fmt.Sprint(len(payload))}}
	contentURL := target + "/api/repository-sync/models/dingo-local/content?" + contentQuery.Encode()
	code, b = syncHTTP(t, "GET", contentURL, nil)
	if code != 200 || !bytes.Equal(b, payload) {
		t.Fatalf("target payload %d %q", code, b)
	}
	if code, b = syncHTTP(t, "POST", endpoint, body); code != 202 {
		t.Fatalf("repeat %d %s", code, b)
	}
	t.Logf("two isolated Speed processes completed %s, commit=%s, bytes=%d", status.TaskID, status.Commit, status.TransferredBytes)
}

// Opt-in isolated acceptance harness. Business handlers, caches and subprocesses
// are real; only the Scheduler's already-selected source lookup is a fixture.
func TestRepositorySyncBrowserHarness(t *testing.T) {
	location := os.Getenv("DINGO_SYNC_BROWSER_READY")
	if location == "" {
		t.Skip("manual browser acceptance only")
	}
	t.Setenv("DINGO_SYNC_BROWSER_MODE", "1")
	source := syncNodeProcess(t, "")
	scheduler := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/upload-inventory/sources" {
			w.WriteHeader(404)
			return
		}
		if _, err := os.Stat(location + ".nosource." + url.QueryEscape(r.URL.Query().Get("repo"))); err == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{{"instanceId": "source", "managementUrl": source}}})
	}))
	if os.Getenv("DINGO_SYNC_BROWSER_PERSIST_ROOT") != "" {
		listener, err := net.Listen("tcp", "127.0.0.1:57517")
		if err != nil {
			t.Fatal(err)
		}
		scheduler.Listener.Close()
		scheduler.Listener = listener
	}
	scheduler.Start()
	defer scheduler.Close()
	target := syncNodeProcess(t, scheduler.URL)
	payload := bytes.Repeat([]byte("actual-browser-sync-payload-012345"), 2<<20)
	digest := sha256.Sum256(payload)
	sha := hex.EncodeToString(digest[:])
	q := url.Values{"repo": {"demo"}, "revision": {"release"}, "path": {"model.bin"}, "sha256": {sha}, "size": {fmt.Sprint(len(payload))}}
	status, body := syncHTTP(t, "POST", source+"/api/uploads/models/datacanvas?"+q.Encode(), payload)
	if status != 201 {
		t.Fatalf("seed: %d %s", status, body)
	}
	ready := map[string]any{"sourceURL": source, "targetURL": target, "schedulerFixtureURL": scheduler.URL, "repoType": "models", "namespace": "datacanvas", "repo": "demo", "revision": "release", "files": []map[string]any{{"path": "model.bin", "sha256": sha, "size": len(payload)}}}
	raw, _ := json.MarshalIndent(ready, "", "  ")
	if err := os.WriteFile(location, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(location + ".stop"); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
}

type slowBrowserBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b *slowBrowserBody) Read(p []byte) (int, error) {
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-time.After(4 * time.Millisecond):
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	return b.ReadCloser.Read(p)
}

// Real Scheduler counterpart is TestRepositorySyncBrowserHarness in Scheduler.
func TestRepositorySyncEntryBrowserHarness(t *testing.T) {
	location := os.Getenv("DINGO_SYNC_BROWSER_READY")
	scheduler := os.Getenv("DINGO_SYNC_BROWSER_SCHEDULER")
	if location == "" || scheduler == "" {
		t.Skip("manual real Scheduler browser acceptance")
	}
	t.Setenv("DINGO_SYNC_BROWSER_MODE", "1")
	origin, remoteFiles := syncBrowserNativeOrigin(t)
	t.Setenv("DINGO_SYNC_BROWSER_ORIGIN", origin)
	nodes := map[string]string{}
	for _, role := range []string{"source", "target", "target2"} {
		nodes[role] = syncNodeProcessRole(t, scheduler, role)
		raw, _ := json.Marshal(map[string]string{"instanceId": role, "url": nodes[role]})
		status, body := syncHTTP(t, "POST", scheduler+"/test/register", raw)
		if status != 200 {
			t.Fatalf("register %s: %d %s", role, status, body)
		}
	}
	payload := bytes.Repeat([]byte("actual-browser-sync-payload-012345"), 2<<20)
	sum := sha256.Sum256(payload)
	sha := hex.EncodeToString(sum[:])
	items := []map[string]any{}
	for _, repo := range []string{"demo", "demo-conflict", "demo-missing"} {
		q := url.Values{"repo": {repo}, "revision": {"release"}, "path": {"model.bin"}, "sha256": {sha}, "size": {fmt.Sprint(len(payload))}}
		status, body := syncHTTP(t, "POST", nodes["source"]+"/api/uploads/models/datacanvas?"+q.Encode(), payload)
		if status != 201 {
			t.Fatalf("seed %s: %d %s", repo, status, body)
		}
		if repo != "demo-missing" {
			items = append(items, map[string]any{"namespace": "datacanvas", "repoType": "models", "repo": repo, "path": "model.bin", "sha256": sha, "size": len(payload)})
		}
	}

	// Existing same-name revision has stale and extra paths; another revision must survive.
	for _, role := range []string{"target", "target2"} {
		for _, repo := range []string{"demo", "demo-conflict"} {
			for _, seed := range []struct{ revision, path, body string }{{"release", "model.bin", "old target body"}, {"release", "target-only.txt", "remove this reference"}, {"keep", "keep.txt", "keep other revision"}} {
				content := []byte(seed.body)
				sum := sha256.Sum256(content)
				q := url.Values{"repo": {repo}, "revision": {seed.revision}, "path": {seed.path}, "sha256": {hex.EncodeToString(sum[:])}, "size": {fmt.Sprint(len(content))}}
				code, body := syncHTTP(t, "POST", nodes[role]+"/api/uploads/models/datacanvas?"+q.Encode(), content)
				if code != 201 {
					t.Fatalf("seed target %s/%s: %d %s", role, repo, code, body)
				}
			}
		}
	}
	now := time.Now().UTC()
	report, _ := json.Marshal(map[string]any{"version": 1, "instanceId": "source", "epoch": "browser-seed", "epochStartedAt": now, "sequence": 1, "generatedAt": now, "complete": true, "items": items})
	status, body := syncHTTP(t, "POST", scheduler+"/test/inventory", report)
	if status != 200 {
		t.Fatalf("inventory %d %s", status, body)
	}

	// Seed an official definition through the real authenticated Scheduler action;
	// Scheduler re-reads and verifies source bytes before accepting this selection.
	for _, repo := range []string{"demo", "demo-conflict"} {
		code, raw := syncHTTP(t, "GET", nodes["source"]+"/api/repositories/models/datacanvas/snapshot?repo="+repo+"&revision=release&verify=sha256", nil)
		var snap struct {
			Commit string `json:"commit"`
		}
		if code != 200 || json.Unmarshal(raw, &snap) != nil || snap.Commit == "" {
			t.Fatalf("source snapshot %d %s", code, raw)
		}
		request, _ := json.Marshal(map[string]any{"namespace": "datacanvas", "repoType": "models", "repo": repo, "action": "commit", "revision": "release", "baseVersion": 0, "requestId": "browser-seed-" + repo, "actor": "sync-reviewer", "changes": []map[string]any{{"path": "model.bin", "file": map[string]any{"path": "model.bin", "sha256": sha, "size": len(payload)}, "source": map[string]string{"node": "source", "revision": "release", "commit": snap.Commit}}}})
		req, _ := http.NewRequest("POST", scheduler+"/internal/v1/official-revisions", bytes.NewReader(request))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer isolated-sync-review")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("official seed %d %s", response.StatusCode, result)
		}
	}

	for _, ns := range []string{"huggingface", "modelscope"} {
		id := "seed-native-" + ns
		files := append([]dao.RemoteSyncFile{}, remoteFiles...)
		if ns == "modelscope" {
			for i := range files {
				files[i].UpstreamFileRevision = strings.Repeat("c", 40)
			}
		}
		req, _ := json.Marshal(service.RemoteSyncRequest{TaskID: id, Repo: "org/remote-demo", Revision: "main", ExpectedAbsent: true, SourceCommit: strings.Repeat("c", 40), Files: files})
		code, body := syncHTTP(t, "POST", nodes["source"]+"/api/repository-sync/models/"+ns+"/remote-prepare", req)
		if code != 200 {
			t.Fatalf("native prepare %s %d %s", ns, code, body)
		}
		code, body = syncHTTP(t, "POST", nodes["source"]+"/api/repository-sync/tasks/"+id+"/execute", nil)
		if code != 200 {
			t.Fatalf("native execute %d %s", code, body)
		}
		deadline := time.Now().Add(90 * time.Second)
		for {
			code, body = syncHTTP(t, "GET", nodes["source"]+"/api/repository-sync/tasks/"+id, nil)
			var st service.SyncStatus
			_ = json.Unmarshal(body, &st)
			if st.State == "completed" {
				break
			}
			if code != 200 || st.State == "failed" || time.Now().After(deadline) {
				t.Fatalf("native seed %s %d %s", ns, code, body)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	ready := map[string]any{"sourceURL": nodes["source"], "targetURL": nodes["target"], "target2URL": nodes["target2"], "schedulerURL": scheduler, "authorityKey": "isolated-sync-review", "originURL": origin, "repoType": "models", "namespace": "datacanvas", "repo": "demo", "revision": "release", "files": []map[string]any{{"path": "model.bin", "sha256": sha, "size": len(payload)}}}
	raw, _ := json.MarshalIndent(ready, "", "  ")
	if err := os.WriteFile(location, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(location + ".stop"); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
}

// Protocol fixture only: target bytes still run through production download/cache/publication.
func syncBrowserNativeOrigin(t *testing.T) (string, []dao.RemoteSyncFile) {
	t.Helper()
	commit := strings.Repeat("c", 40)
	bodies := map[string][]byte{"config.json": []byte(`{"model":"remote-demo"}`), "model.bin": bytes.Repeat([]byte("native-browser-payload-0123456789"), 1<<20)}
	files := []dao.RemoteSyncFile{}
	for _, path := range []string{"config.json", "model.bin"} {
		b := bodies[path]
		sum := sha256.Sum256(b)
		files = append(files, dao.RemoteSyncFile{Path: path, OID: hex.EncodeToString(sum[:]), Size: int64(len(b)), IdentityKind: "sha256"})
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "paths-info") {
			rows := []map[string]any{}
			for _, f := range files {
				rows = append(rows, map[string]any{"path": f.Path, "type": "file", "size": f.Size, "oid": strings.Repeat("e", 40), "lfs": map[string]any{"oid": f.OID, "size": f.Size}})
			}
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			rows := []map[string]any{}
			for _, f := range files {
				rows = append(rows, map[string]any{"Path": f.Path, "Type": "blob", "Sha256": f.OID, "Size": f.Size, "Revision": commit})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Code": 200, "Data": map[string]any{"LatestCommitter": map[string]string{"Id": commit}, "Files": rows}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			rows := []map[string]string{}
			for _, f := range files {
				rows = append(rows, map[string]string{"rfilename": f.Path})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": commit, "siblings": rows})
			return
		}
		path := r.URL.Query().Get("FilePath")
		if path == "" {
			parts := strings.SplitN(r.URL.Path, "/resolve/"+commit+"/", 2)
			if len(parts) == 2 {
				path = parts[1]
			}
		}
		body, ok := bodies[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(origin.Close)
	return origin.URL, files
}
