package dao

import (
	"errors"
	"net/http"
	"strings"
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
	_, err := fileDao.GetFileCommitSha("models", "org/repo", "main", "", "meta")
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
	_, err := fileDao.GetFileCommitSha("models", "org/repo", "main", "", "meta")
	if err == nil {
		t.Fatal("expected inaccessible storage error")
	}
	var accessErr *util.FileAccessError
	if !errors.As(err, &accessErr) {
		t.Fatalf("got %T %v, want wrapped FileAccessError", err, err)
	}
	if accessErr.Kind != util.FileAccessUnavailable {
		t.Fatalf("got kind %q, want %q", accessErr.Kind, util.FileAccessUnavailable)
	}
	if strings.Contains(err.Error(), "invalid\x00repos") {
		t.Fatalf("public application error exposes storage path: %q", err.Error())
	}
}
