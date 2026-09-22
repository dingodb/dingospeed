package cachemount

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"dingospeed/pkg/repository"
)

func treeJSON(t *testing.T, p string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func hostedTree(t *testing.T) (Selection, string) {
	t.Helper()
	root := t.TempDir()
	k := repository.RepoKey{Namespace: "datacanvas", RepoType: "models", Repo: "team/model"}
	treeJSON(t, filepath.Join(k.APIRoot(root), repository.Marker), repository.Hosted(k))
	body, _ := json.Marshal(map[string]any{"sha": "commit1", "siblings": []any{map[string]any{"rfilename": "nested/config.json"}}})
	meta := map[string]any{"status_code": 200, "content": hex.EncodeToString(body)}
	treeJSON(t, filepath.Join(k.Revision(root, "main"), "meta_get.json"), meta)
	treeJSON(t, filepath.Join(k.Revision(root, "commit1"), "meta_get.json"), meta)
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	treeJSON(t, filepath.Join(k.Revision(root, "commit1"), "dingo-local-manifest.json"), []any{map[string]any{"path": "nested/config.json", "sha256": sha, "size": 8}})
	data, err := os.ReadFile(fixture(t, []byte("abcdefgh")))
	if err != nil {
		t.Fatal(err)
	}
	p := k.Blob(root, sha)
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	return Selection{root, k.Namespace, k.Repo, "main"}, p
}

func TestTreeDiscoveryAndPinnedVersion(t *testing.T) {
	s, _ := hostedTree(t)
	choices, err := Discover(s.CacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(choices) != 1 || choices[0] != s {
		t.Fatalf("unexpected choices: %+v", choices)
	}
	snapshot, versions, err := PrepareTree(choices, "/mnt/dingo-models")
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if versions[0].Status != "ready" || versions[0].SourceFilePath != "/mnt/dingo-models/datacanvas/team/model/main" {
		t.Fatal(versions)
	}
	// Moving the branch metadata must not change the already mounted snapshot.
	k := repository.RepoKey{Namespace: s.Namespace, RepoType: "models", Repo: s.Repo}
	body, _ := json.Marshal(map[string]string{"sha": "anothercommit"})
	treeJSON(t, filepath.Join(k.Revision(s.CacheRoot, "main"), "meta_get.json"), map[string]any{"status_code": 200, "content": hex.EncodeToString(body)})
	b := make([]byte, 8)
	_, err = snapshot.Files["datacanvas/team/model/main/nested/config.json"].ReadAt(b, 0)
	if err != nil || !bytes.Equal(b, []byte("abcdefgh")) {
		t.Fatalf("pinned bytes changed: %q %v", b, err)
	}
}

func TestTreeIncompleteVersionIsAtomic(t *testing.T) {
	s, p := hostedTree(t)
	if err := os.Truncate(p, 38); err != nil {
		t.Fatal(err)
	}
	snapshot, versions, err := PrepareTree([]Selection{s}, "/mnt/dingo-models")
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if versions[0].Status != "unavailable" || len(snapshot.Files) != 0 || snapshot.Directories["datacanvas/team/model/main"] == "" {
		t.Fatalf("partial version exposed: %+v", versions)
	}
}

func TestTreeRejectsAmbiguousAndUnsafeSelections(t *testing.T) {
	s, _ := hostedTree(t)
	duplicate := []Selection{s, s}
	if snapshot, _, err := PrepareTree(duplicate, "/mnt/dingo-models"); err == nil {
		snapshot.Close()
		t.Fatal("accepted duplicate")
	}
	s.Revision = "../escape"
	if snapshot, _, err := PrepareTree([]Selection{s}, "/mnt/dingo-models"); err == nil {
		snapshot.Close()
		t.Fatal("accepted traversal")
	}
}

func TestPlainHFCannotHideCorruptContainer(t *testing.T) {
	name := filepath.Join(t.TempDir(), "plain")
	os.WriteFile(name, []byte("{}"), 0600)
	p, err := openTreePayload(name, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	p.File.Close()
	if p, err := openTreePayload(name, 3, true); err == nil {
		p.File.Close()
		t.Fatal("accepted wrong size")
	}
	os.WriteFile(name, []byte("OLAH"), 0600)
	if p, err := openTreePayload(name, 4, true); err == nil {
		p.File.Close()
		t.Fatal("corrupt OLAH accepted as raw")
	}
}
