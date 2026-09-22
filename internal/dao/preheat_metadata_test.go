package dao

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
)

func TestRepeatedPreheatRefreshesMainAndPreservesSnapshots(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	commit, calls, status := "commit-a", 0, 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/models/owner/model/revision/main" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"sha":%q,"siblings":[{"rfilename":"config.json"}]}`, commit)
	}))
	defer server.Close()
	config.SysConfig = &config.Config{Server: config.ServerConfig{Online: true, Repos: t.TempDir(), HfScheme: "http", HfNetLoc: strings.TrimPrefix(server.URL, "http://")}, Retry: config.Retry{Attempts: 1}}
	base := data.NewBaseData()
	locks := NewLockDao(base)
	files := NewFileDao(nil, base, locks)
	meta := NewMetaDao(files, locks, base)
	key := repository.RepoKey{Namespace: repository.HuggingFace, RepoType: "models", Repo: "owner/model"}
	for _, next := range []string{"commit-a", "commit-b", "commit-a", "commit-a"} {
		commit = next
		got, err := meta.RefreshPreheatMetadata(key, "main", "")
		if err != nil || got.Sha != next {
			t.Fatalf("refresh=%+v err=%v", got, err)
		}
		local, err := files.GetCommitHfOffline("models", key.ID(), "main")
		if err != nil || local != next {
			t.Fatalf("main=%s err=%v", local, err)
		}
	}
	if calls != 4 {
		t.Fatalf("upstream calls=%d", calls)
	}
	for _, rev := range []string{"commit-a", "commit-b"} {
		if got, err := files.GetCommitHfOffline("models", key.ID(), rev); err != nil || got != rev {
			t.Fatalf("snapshot=%s err=%v", got, err)
		}
	}
	status = 403
	if _, err := meta.RefreshPreheatMetadata(key, "main", ""); err == nil {
		t.Fatal("upstream denial reported success")
	}
	if got, err := files.GetCommitHfOffline("models", key.ID(), "main"); err != nil || got != "commit-a" {
		t.Fatalf("failed refresh changed main: %s %v", got, err)
	}
}
