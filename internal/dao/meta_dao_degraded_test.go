package dao

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
)

func TestRequestAndSaveMetaServesRemoteResponseWhenCacheUnavailable(t *testing.T) {
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
	got, err := metaDao.requestAndSaveMeta("models", "org/repo", "main", "remote-commit", consts.RequestTypeGet, "")
	if err != nil {
		t.Fatalf("remote metadata should survive cache failure: %v", err)
	}
	if string(got.OriginContent) != body {
		t.Fatalf("got body %q, want %q", got.OriginContent, body)
	}
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
	if _, err := metaDao.requestAndSaveMeta("models", "org/repo", "main", "remote-commit", consts.RequestTypeGet, ""); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"main", "remote-commit"} {
		path := filepath.Join(repos, "api", "models", "org", "repo", "revision", revision, "meta_get.json")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected metadata cache %s: %v", path, err)
		}
	}
}
