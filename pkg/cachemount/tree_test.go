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
	if versions[0].Status != "ready" || versions[0].SourceFilePath != filepath.Join("/mnt/dingo-models", "datacanvas", "team", "model", "main") {
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

// Both remote providers use normalized cached metadata, but distinct payload roots.
func TestRemoteProviderTree(t *testing.T) {
	for _, namespace := range []string{repository.HuggingFace, repository.ModelScope} {
		t.Run(namespace, func(t *testing.T) {
			root := t.TempDir()
			k := repository.RepoKey{Namespace: namespace, RepoType: "models", Repo: "team/model"}
			treeJSON(t, filepath.Join(k.APIRoot(root), repository.Marker), repository.Remote(k))
			body, _ := json.Marshal(map[string]any{"sha": "commit1", "siblings": []any{map[string]any{"rfilename": "nested/config.json", "size": 8, "blobId": "blob1"}}})
			for _, rev := range []string{"master", "commit1"} {
				treeJSON(t, filepath.Join(k.Revision(root, rev), "meta_get.json"), map[string]any{"status_code": 200, "content": hex.EncodeToString(body)})
			}
			backing := k.Blob(root, "blob1")
			data, err := os.ReadFile(fixture(t, []byte("abcdefgh")))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(filepath.Dir(backing), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(backing, data, 0600); err != nil {
				t.Fatal(err)
			}
			selections, err := Discover(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(selections) != 1 || selections[0].Namespace != namespace || selections[0].Revision != "master" {
				t.Fatalf("unexpected discovery: %+v", selections)
			}
			snapshot, versions, err := PrepareTree(selections, filepath.Join(root, "view"))
			if err != nil {
				t.Fatal(err)
			}
			prefix := namespace + "/team/model/master"
			if len(versions) != 1 || versions[0].Status != "ready" || versions[0].Commit != "commit1" {
				snapshot.Close()
				t.Fatalf("not ready: %+v", versions)
			}
			if versions[0].Files[0].Backing != backing {
				t.Fatalf("wrong backing: %+v", versions)
			}
			payload := snapshot.Files[prefix+"/nested/config.json"]
			if payload == nil {
				snapshot.Close()
				t.Fatal("missing virtual file")
			}
			b := make([]byte, 8)
			_, err = payload.ReadAt(b, 0)
			snapshot.Close()
			if err != nil || string(b) != "abcdefgh" {
				t.Fatalf("payload: %q %v", b, err)
			}
			if err = os.Remove(backing); err != nil {
				t.Fatal(err)
			}
			snapshot, versions, err = PrepareTree(selections, filepath.Join(root, "view"))
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Close()
			if versions[0].Status != "unavailable" || len(snapshot.Files) != 0 {
				t.Fatalf("exposed incomplete version: %+v", versions)
			}
		})
	}
}
