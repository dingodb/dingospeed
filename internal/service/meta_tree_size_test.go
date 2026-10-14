package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/pkg/config"
)

// 2026-10-14: an empty .gitkeep lost its size in the tree response, so the
// scheduler rejected CASIA-LM/ChineseWebText2.0 with "invalid snapshot content
// identity" and the completed dataset was never registered.
func TestRepoTreeKeepsZeroFileSize(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/tree/") {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `[{"type":"directory","path":"d","oid":"tree-oid","size":0},`+
			`{"type":"file","path":"d/.gitkeep","oid":"e69de29bb2d1d6434b8b29ae775ad8c2e48c5391","size":0}]`)
	}))
	defer upstream.Close()
	msConfig(t, upstream.URL)
	config.SysConfig.Server.HfScheme = "http"
	config.SysConfig.Server.HfNetLoc = strings.TrimPrefix(upstream.URL, "http://")
	base := data.NewBaseData()
	locks := dao.NewLockDao(base)
	fileDao := dao.NewFileDao(dao.NewDownloaderDao(nil), base, locks)
	meta := NewMetaService(fileDao, dao.NewMetaDao(fileDao, locks, base))

	tree, err := meta.GetRepoTree("datasets", "huggingface/owner/data", "commit-1", "", true, "")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item["path"] == "d/.gitkeep" {
			if size, ok := item["size"]; !ok || size != float64(0) {
				t.Fatalf("empty file size missing from tree: %s", body)
			}
			return
		}
	}
	t.Fatalf("empty file missing from tree: %s", body)
}
