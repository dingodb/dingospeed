package dao

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/util"
)

func TestGetCommitHfOfflinePreservesMissingBehavior(t *testing.T) {
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir()}}
	t.Cleanup(func() { config.SysConfig = oldConfig })

	fileDao := NewFileDao(nil, data.NewBaseData(), nil)
	_, err := fileDao.GetFileCommitSha("models", "huggingface/org/repo", "main", "", "meta")
	var appErr myerr.Error
	if !errors.As(err, &appErr) || appErr.StatusCode() != http.StatusNotFound {
		t.Fatalf("missing metadata: got %v, want HTTP 404 application error", err)
	}
}

func TestGetFileCommitShaPreservesStorageFailure(t *testing.T) {
	oldConfig := config.SysConfig
	// A NUL byte makes stat fail as an invalid path on every supported OS. It
	// exercises the inaccessible-path branch without relying on host mounts.
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: "invalid\x00repos"}}
	t.Cleanup(func() { config.SysConfig = oldConfig })

	fileDao := NewFileDao(nil, data.NewBaseData(), nil)
	_, err := fileDao.GetFileCommitSha("models", "huggingface/org/repo", "main", "", "meta")
	if err == nil {
		t.Fatal("expected inaccessible storage error")
	}
	var appErr myerr.Error
	var access *util.FileAccessError
	if !errors.As(err, &appErr) || appErr.StatusCode() != 503 || !errors.As(err, &access) {
		t.Fatalf("storage failure lost: %v", err)
	}
}

func TestCommitObservationPreservesFallbackAndReadErrors(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"sha":"remote"}`) }))
	defer server.Close()
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	for _, tc := range []struct {
		name, source, org string
		remote            bool
	}{
		{"offline-meta", "meta", "huggingface/org/repo", false},
		{"offline-file-fallback", "file", "huggingface/org/repo", true},
		{"local-file-no-fallback", "file", "dingo-local/repo", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: "invalid\x00repos", HfScheme: "http", HfNetLoc: strings.TrimPrefix(server.URL, "http://")}, Retry: config.Retry{Attempts: 1}, Upload: config.Upload{Namespace: "dingo-local"}}
			base := data.NewBaseData()
			file := NewFileDao(nil, base, NewLockDao(base))
			before, observations := calls.Load(), util.FileAccessObservations()
			sha, err := file.GetFileCommitSha("models", tc.org, "main", "", tc.source)
			if tc.remote {
				if err != nil || sha != "remote" || calls.Load()-before != 1 {
					t.Fatalf("fallback changed: sha=%s err=%v calls=%d", sha, err, calls.Load()-before)
				}
			} else {
				var app myerr.Error
				if !errors.As(err, &app) || app.StatusCode() != 503 || calls.Load() != before {
					t.Fatalf("local response changed: %v", err)
				}
			}
			assertObservationDelta(t, observations, "stat")
		})
	}
	t.Run("stat-success-read-failure", func(t *testing.T) {
		config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir()}}
		base := data.NewBaseData()
		file := NewFileDao(nil, base, NewLockDao(base))
		path := filepath.Join(config.SysConfig.Repos(), "api/models/org/repo/revision/main/meta_get.json")
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		_, original := file.ReadCacheRequest(path)
		before := util.FileAccessObservations()
		_, got := file.GetCommitHfOffline("models", "huggingface/org/repo", "main")
		var app myerr.Error
		if !errors.As(got, &app) || got.Error() != original.Error() {
			t.Fatalf("read error contract changed: %v / %v", got, original)
		}
		assertObservationDelta(t, before, "read")
	})
}

func assertObservationDelta(t *testing.T, before []util.FileAccessObservation, operation string) {
	t.Helper()
	var delta uint64
	for i, value := range util.FileAccessObservations() {
		if value.Operation == operation {
			delta += value.Count - before[i].Count
		} else if value.Count != before[i].Count {
			t.Fatalf("unexpected operation: %+v", value)
		}
	}
	if delta != 1 {
		t.Fatalf("want one %s observation, got %d", operation, delta)
	}
}

func TestLocalMetadataErrorPreservesCauseAndHidesPath(t *testing.T) {
	for _, cause := range []error{os.ErrPermission, syscall.Errno(5), syscall.Errno(107)} {
		original := &os.PathError{Op: "read", Path: "private/cache/meta_get.json", Err: cause}
		got := localMetadataError(myerr.Wrap("read failed", original))
		var app myerr.Error
		if !errors.As(got, &app) || app.StatusCode() != 503 || !errors.Is(got, cause) {
			t.Fatalf("access failure contract: %v", got)
		}
		if strings.Contains(got.Error(), original.Path) {
			t.Fatal("private path exposed")
		}
	}
}

func TestHostedMetadataUsesLocalErrorsEvenWhenOnline(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	for _, online := range []bool{false, true} {
		config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir(), Online: online}, Upload: config.Upload{Namespace: "dingo-local"}}
		base := data.NewBaseData()
		lock := NewLockDao(base)
		file := NewFileDao(nil, base, lock)
		base.Cache.Set(GetMetaShaRepoKey("models/dingo-local/repo", "main", ""), "abc123", 0)
		meta := NewMetaDao(file, lock, base)
		_, err := meta.GetMetadata("models", "dingo-local/repo", "main", "get", "")
		var app myerr.Error
		if !errors.As(err, &app) || app.StatusCode() != 404 {
			t.Fatalf("online=%v missing: %v", online, err)
		}
		p := filepath.Join(config.SysConfig.Repos(), "api/models/dingo-local/repo/revision/abc123/meta_get.json")
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		_, err = meta.GetMetadata("models", "dingo-local/repo", "main", "get", "")
		if !errors.As(err, &app) || app.StatusCode() != 503 {
			t.Fatalf("online=%v read failure: %v", online, err)
		}
	}
}
