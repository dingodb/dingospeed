package hfprojection

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMountSourcesPinsOneManifest(t *testing.T) {
	r, _ := fixture(t)
	blob(t, filepath.Join(r.Root, "files", "models", "Qwen", "demo", "blobs", "missing"), 9, 7)
	m, paths, err := r.MountSources("models", "Qwen/demo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if m.Commit != "commit1" || len(paths) != 3 {
		t.Fatalf("unexpected sources: %+v %v", m, paths)
	}
	if paths["weights.bin"] != filepath.Join(r.Root, "files", "models", "Qwen", "demo", "blobs", "weight") {
		t.Fatal(paths)
	}
}

func TestMountSourcesRequiresCompleteMetadata(t *testing.T) {
	r, api := fixture(t)
	if err := os.Chmod(filepath.Join(api, "revision", "main", "meta_get.json"), 0600); err != nil {
		t.Fatal(err)
	}
	response(t, filepath.Join(api, "revision", "main", "meta_get.json"), map[string]any{"sha": "commit1"})
	if _, _, err := r.MountSources("models", "Qwen/demo", "main"); err == nil {
		t.Fatal("accepted incomplete manifest")
	}
}

func TestMountSourcesRejectsEscapingLink(t *testing.T) {
	r, _ := fixture(t)
	blob(t, filepath.Join(r.Root, "files", "models", "Qwen", "demo", "blobs", "missing"), 9, 7)
	p := filepath.Join(r.Root, "files", "models", "Qwen", "demo", "blobs", "weight")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "other")
	blob(t, outside, 8, 3)
	if err := os.Symlink(outside, p); err != nil {
		t.Skip(err)
	}
	if _, _, err := r.MountSources("models", "Qwen/demo", "main"); err == nil {
		t.Fatal("accepted escaping blob")
	}
}
