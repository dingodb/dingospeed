package router

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"dingospeed/pkg/config"
)

func TestHFProjectionHTTPDoesNotRegisterOrDownload(t *testing.T) {
	e, _ := namespaceEngines(t)
	// The server is online, but a projection request must never use its remote
	// read-through path (which would register a descriptor or access upstream).
	config.SysConfig.Server.Online = true
	root := config.SysConfig.Repos()
	api := filepath.Join(root, "api", "models", "team", "demo")
	meta := filepath.Join(api, "revision", "main", "meta_get.json")
	os.MkdirAll(filepath.Dir(meta), 0755)
	body := []byte(`{"sha":"commit1","siblings":[{"rfilename":"README.md","size":5,"blobId":"readme"},{"rfilename":"missing.bin","size":9,"blobId":"missing"}]}`)
	wrapped, _ := json.Marshal(map[string]any{"status_code": 200, "content": hex.EncodeToString(body)})
	if err := os.WriteFile(meta, wrapped, 0444); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(root, "files", "models", "team", "demo", "blobs", "readme")
	os.MkdirAll(filepath.Dir(blob), 0755)
	if err := os.WriteFile(blob, []byte("hello"), 0444); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"local-catalog", "local-manifest?repo=team%2Fdemo&revision=main", "local-file?repo=team%2Fdemo&revision=main&path=README.md"} {
		r := namespaceRequest(e, http.MethodGet, "/api/repositories/models/huggingface/"+route, nil)
		requireStatus(t, r, 200)
	}
	r := namespaceRequest(e, http.MethodGet, "/api/repositories/models/huggingface/local-file?repo=team%2Fdemo&revision=main&path=missing.bin", nil)
	requireStatus(t, r, 404)
	r = namespaceRequest(e, http.MethodGet, "/api/repositories/models/huggingface/local-manifest?repo=team%2Fdemo&revision=absent", nil)
	requireStatus(t, r, 404)
	r = namespaceRequest(e, http.MethodGet, "/api/repositories/models/datacanvas/local-catalog", nil)
	requireStatus(t, r, 400)
	r = namespaceRequest(e, http.MethodGet, "/api/repositories/models/huggingface/local-manifest?repo=team%2Fdemo&repo=other&revision=main", nil)
	requireStatus(t, r, 400)
	if _, err := os.Stat(filepath.Join(api, "repository.json")); !os.IsNotExist(err) {
		t.Fatal("read registered HF repository")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(blob), "missing")); !os.IsNotExist(err) {
		t.Fatal("missing file was fetched")
	}
}
