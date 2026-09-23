package repository

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegisteredRepositoryDoesNotEnumerateUnrelatedMarkers(t *testing.T) {
	root := t.TempDir()
	d := Hosted(RepoKey{"alice", "models", "existing"})
	if err := Register(root, d); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(root, "api", "models", Local, "alice", "unrelated", Marker)
	if err := os.MkdirAll(filepath.Dir(bad), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("invalid"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Register(root, d); err != nil {
		t.Fatalf("idempotent registration scanned unrelated repository: %v", err)
	}
	if err := Register(root, Hosted(RepoKey{"alice", "models", "new"})); err == nil {
		t.Fatal("new registration bypassed conflict discovery")
	}
	if err := os.WriteFile(filepath.Join(d.APIRoot(root), Marker), []byte("invalid"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Register(root, d); err == nil {
		t.Fatal("accepted damaged own descriptor")
	}
}
