package repository

import (
	"os"
	"path/filepath"
	"testing"
)

// This probe documents the ambiguity that constrains the pruning rule. Two
// on-disk situations are byte-for-byte indistinguishable without extra
// information:
//
//	A. orphan residue: api/models/alice/ghost/paths-info/... where the marker was
//	   deleted with its repository
//	B. a legitimate repository whose own name begins with a reserved word, i.e.
//	   api/models/alice/<repo>/paths-info/... where <repo> == "paths-info/repo"
//
// In (B) the directory named "paths-info" is a repository root's ancestor
// segment, not data, and pruning it would lose a real repository. The test
// asserts both trees can coexist and that only the marker-bearing one is found.
func TestPathsInfoAmbiguityIsResolvedByMarkerOnly(t *testing.T) {
	root := t.TempDir()
	// (B) a legitimate repository whose first segment is "paths-info".
	legit := RepoKey{Namespace: "alice", RepoType: "models", Repo: "paths-info/repo"}
	if err := Register(root, Hosted(legit)); err != nil {
		t.Fatal(err)
	}
	// (A) orphan residue under a sibling namespace.
	orphan := filepath.Join(root, "api", "models", "alice", "ghost")
	if err := os.MkdirAll(filepath.Join(orphan, "paths-info", "commit", "w", "f.bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	all, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the legitimate repository is found; the orphan contributes nothing
	// and must not error either.
	if len(all) != 1 {
		t.Fatalf("expected only the marker-bearing repository, got %+v", all)
	}
	if all[0].RepoKey != legit {
		t.Fatalf("wrong repository discovered: %+v", all[0])
	}
}