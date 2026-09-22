package task

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/model/query"
	"dingospeed/pkg/config"
)

func TestPreheatReusedBlobGetsNewCommitReference(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.Contains(r.URL.Path, "/paths-info/") {
			t.Errorf("unexpected download %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		fmt.Fprint(w, `[{"type":"file","path":"model.bin","oid":"shared","size":8}]`)
	}))
	defer upstream.Close()
	config.SysConfig = &config.Config{Server: config.ServerConfig{Online: true, Repos: t.TempDir(), HfScheme: "http", HfNetLoc: strings.TrimPrefix(upstream.URL, "http://")}, Retry: config.Retry{Attempts: 1}, Download: config.Download{BlockSize: 4}}
	base := data.NewBaseData()
	files := dao.NewFileDao(nil, base, dao.NewLockDao(base))
	blob := dao.BlobPath("models", "huggingface/owner/model", "shared")
	content := make([]byte, 45)
	copy(content, "OLAH")
	binary.LittleEndian.PutUint64(content[4:], 8)
	binary.LittleEndian.PutUint64(content[12:], 4)
	binary.LittleEndian.PutUint64(content[20:], 8)
	binary.LittleEndian.PutUint64(content[28:], 8)
	content[36] = 3
	copy(content[37:], "abcdefgh")
	if err := os.MkdirAll(filepath.Dir(blob), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, content, 0600); err != nil {
		t.Fatal(err)
	}
	for _, commit := range []string{"old-commit", "new-commit", "new-commit"} {
		var meta dao.CommitHfSha
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"sha":%q,"siblings":[{"rfilename":"model.bin"}]}`, commit)), &meta)
		p := PreheatCacheTask{CacheTask: CacheTask{Ctx: context.Background(), Job: &query.CreateCacheJobReq{Namespace: "huggingface", Repo: "owner/model", Datatype: "models"}}, Sha: &meta, FileDao: files}
		if err := p.preheatProcess("huggingface/owner/model"); err != nil {
			t.Fatal(err)
		}
		if p.stockLen.Load() != 8 {
			t.Fatalf("reused=%d", p.stockLen.Load())
		}
		got, err := os.ReadFile(dao.ResolvePath("models", "huggingface/owner/model", commit, "model.bin"))
		if err != nil || string(got) != string(content) {
			t.Fatalf("reference err=%v bytes=%d", err, len(got))
		}
	}
}
