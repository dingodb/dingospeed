package dao

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/util"
)

func TestRequestAndSaveMetaReturnsUpstreamWhenCacheUnavailable(t *testing.T) {
	const body = `{"sha":"remote-commit","id":"org/repo"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{
		Server: config.ServerConfig{
			Online:   true,
			Repos:    "invalid\x00repos",
			HfScheme: "http",
			HfNetLoc: strings.TrimPrefix(server.URL, "http://"),
		},
		Retry: config.Retry{Attempts: 1},
	}
	t.Cleanup(func() { config.SysConfig = oldConfig })

	baseData := data.NewBaseData()
	fileDao := NewFileDao(nil, baseData, NewLockDao(baseData))
	metaDao := NewMetaDao(fileDao, nil, baseData)
	before := util.FileAccessObservations()
	got, err := metaDao.requestAndSaveMeta("models", "huggingface/org/repo", "main", "remote-commit", consts.RequestTypeGet, "")
	if err != nil || got == nil || got.StatusCode != 200 || string(got.OriginContent) != body {
		t.Fatalf("successful upstream response lost: content=%v err=%v", got, err)
	}
	assertObservationDelta(t, before, "mkdir")
}

func TestMetadataCacheWriteObservationPreservesOriginalError(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir()}}
	base := data.NewBaseData()
	file := NewFileDao(nil, base, NewLockDao(base))
	meta := NewMetaDao(file, nil, base)
	path := filepath.Join(config.SysConfig.Repos(), "api/models/org/repo/revision/main/meta_get.json")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	before := util.FileAccessObservations()
	err := meta.writeApiMetaFile("models", "huggingface/org/repo", "main", "get", 200, nil, []byte(`{"sha":"abc"}`))
	if err == nil {
		t.Fatal("expected atomic replacement failure")
	}
	assertObservationDelta(t, before, "write")
}

func TestRequestAndSaveMetaStillCachesWhenStorageAvailable(t *testing.T) {
	const body = `{"sha":"remote-commit","id":"org/repo"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	repos := t.TempDir()
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{
		Server: config.ServerConfig{
			Online:   true,
			Repos:    repos,
			HfScheme: "http",
			HfNetLoc: strings.TrimPrefix(server.URL, "http://"),
		},
		Retry: config.Retry{Attempts: 1},
	}
	t.Cleanup(func() { config.SysConfig = oldConfig })

	baseData := data.NewBaseData()
	fileDao := NewFileDao(nil, baseData, NewLockDao(baseData))
	metaDao := NewMetaDao(fileDao, nil, baseData)
	if _, err := metaDao.requestAndSaveMeta("models", "huggingface/org/repo", "main", "remote-commit", consts.RequestTypeGet, ""); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"main", "remote-commit"} {
		path := filepath.Join(repos, "api", "models", "org", "repo", "revision", revision, "meta_get.json")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected metadata cache %s: %v", path, err)
		}
	}
}

func TestOnlineMetadataCacheFailureAtEachWrite(t *testing.T) {
	const body = `{"sha":"remote-commit"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fixture"`)
		if r.Method != "HEAD" {
			_, _ = w.Write([]byte(body))
		}
	}))
	defer server.Close()
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	for _, revision := range []string{"main", "v1"} {
		for _, target := range []string{"main", "remote-commit"} {
			for _, method := range []string{"get", "head"} {
				t.Run(revision+"/"+target+"/"+method, func(t *testing.T) {
					config.SysConfig = &config.Config{Server: config.ServerConfig{Online: true, Repos: t.TempDir(), HfScheme: "http", HfNetLoc: strings.TrimPrefix(server.URL, "http://")}, Retry: config.Retry{Attempts: 1}}
					p := filepath.Join(config.SysConfig.Repos(), "api/models/org/repo/revision", target, "meta_"+method+".json")
					if err := os.MkdirAll(p, 0700); err != nil {
						t.Fatal(err)
					}
					// For a non-main revision, a pre-existing main cache is deliberately
					// skipped; remove this case from failure-count assertions.
					base := data.NewBaseData()
					file := NewFileDao(nil, base, NewLockDao(base))
					meta := NewMetaDao(file, nil, base)
					got, err := meta.requestAndSaveMeta("models", "huggingface/org/repo", revision, "remote-commit", method, "")
					if err != nil || got == nil || got.StatusCode != 200 {
						t.Fatalf("response lost: %v", err)
					}
					if method == "get" && string(got.OriginContent) != body {
						t.Fatal("upstream body altered")
					}
					if method == "head" && len(got.OriginContent) != 0 {
						t.Fatal("HEAD body altered")
					}
					if info, err := os.Stat(p); err != nil || !info.IsDir() {
						t.Fatal("existing object replaced")
					}
				})
			}
		}
	}
}

func TestIncompleteUpstreamMetadataStillFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
	}))
	defer server.Close()
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	config.SysConfig = &config.Config{Server: config.ServerConfig{Online: true, Repos: "invalid\x00repos", HfScheme: "http", HfNetLoc: strings.TrimPrefix(server.URL, "http://")}, Retry: config.Retry{Attempts: 1}}
	base := data.NewBaseData()
	file := NewFileDao(nil, base, NewLockDao(base))
	meta := NewMetaDao(file, nil, base)
	if got, err := meta.requestAndSaveMeta("models", "huggingface/org/repo", "main", "commit", "get", ""); err == nil || got != nil {
		t.Fatalf("incomplete upstream masked: %v %v", got, err)
	}
}

func TestMetadataCacheToleranceDoesNotMaskValidation(t *testing.T) {
	for _, err := range []error{errors.New("namespace case conflict"), errors.New("repository registration busy"), errors.New("symlink in repository path")} {
		if metadataCacheIOError(err) {
			t.Fatalf("non-storage error tolerated: %v", err)
		}
	}
	for _, err := range []error{&os.PathError{Op: "write", Err: os.ErrNotExist}, &os.LinkError{Op: "rename", Err: os.ErrPermission}, syscall.Errno(5)} {
		if !metadataCacheIOError(err) {
			t.Fatalf("cache persistence error not recognized: %v", err)
		}
	}
}
