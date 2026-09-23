package repository

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// countVisits measures how many directory entries are opened by a walk, which is
// what actually costs FUSE round trips on a network filesystem.
func countVisits(t *testing.T, scanRoot string) (dirs int, files int) {
	t.Helper()
	err := filepath.WalkDir(scanRoot, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.IsDir() {
			dirs++
		} else {
			files++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return dirs, files
}

// buildHeavyNode models a production node: a few hosted repositories plus a large
// remote cache whose repositories each carry a deep per-file paths-info tree.
func buildHeavyNode(t *testing.T, hostedCount, remoteCount, filesPerRepo int) string {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < hostedCount; i++ {
		k := RepoKey{Namespace: "datacanvas", RepoType: "models", Repo: "team/hosted-" + strconv.Itoa(i)}
		if err := Register(root, Hosted(k)); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < filesPerRepo; j++ {
			writeFixture(t, filepath.Join(k.PathsInfo(root, "commit1", "dir/file-"+strconv.Itoa(j)), "paths-info_post.json"))
		}
	}
	for i := 0; i < remoteCount; i++ {
		k := RepoKey{Namespace: HuggingFace, RepoType: "models", Repo: "Qwen/cached-" + strconv.Itoa(i)}
		if err := Register(root, Remote(k)); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < filesPerRepo; j++ {
			writeFixture(t, filepath.Join(k.PathsInfo(root, "commit1", "dir/file-"+strconv.Itoa(j)), "paths-info_post.json"))
		}
	}
	return root
}

func writeFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The hosted-scoped scan must return the same descriptors as filtering List,
// while touching dramatically fewer directories.
func TestListHostedIsEquivalentButFarCheaper(t *testing.T) {
	const hosted = 3
	const remote = 20
	const filesPerRepo = 25
	root := buildHeavyNode(t, hosted, remote, filesPerRepo)

	// Ground truth: the pre-change behaviour, filtered to hosted.
	all, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Descriptor{}
	for _, d := range all {
		if d.Source == "hosted" {
			want[d.ID()] = d
		}
	}
	if len(want) != hosted {
		t.Fatalf("fixture built %d hosted repositories, List saw %d", hosted, len(want))
	}

	got, err := ListHosted(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListHosted returned %d, want %d", len(got), len(want))
	}
	for _, d := range got {
		if want[d.ID()] != d {
			t.Fatalf("descriptor mismatch for %s: %+v", d.ID(), d)
		}
	}

	// Cost comparison: full api/ walk versus hosted-only walk.
	fullDirs, fullFiles := countVisits(t, filepath.Join(root, "api"))
	hostedDirs, hostedFiles := countVisits(t, filepath.Join(root, "api", "models", Local))

	t.Logf("full api/ walk: %d dirs, %d files", fullDirs, fullFiles)
	t.Logf("hosted root walk: %d dirs, %d files", hostedDirs, hostedFiles)

	if hostedDirs >= fullDirs {
		t.Fatalf("hosted scan visited %d dirs, not fewer than full scan %d", hostedDirs, fullDirs)
	}
	if hostedFiles >= fullFiles {
		t.Fatalf("hosted scan visited %d files, not fewer than full scan %d", hostedFiles, fullFiles)
	}
}

// The whole point of the change: a node that caches heavily must not pay for the
// cache when reporting uploads. Remote repositories must contribute nothing.
func TestListHostedCostIsIndependentOfRemoteCacheSize(t *testing.T) {
	small := buildHeavyNode(t, 2, 2, 10)
	large := buildHeavyNode(t, 2, 60, 10)

	for _, c := range []struct{ name, root string }{{"small", small}, {"large", large}} {
		got, err := ListHosted(c.root)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != 2 {
			t.Fatalf("%s: hosted enumeration returned %d, want 2", c.name, len(got))
		}
		for _, d := range got {
			if d.Source != "hosted" {
				t.Fatalf("%s: non-hosted descriptor leaked: %+v", c.name, d)
			}
		}
	}

	_, smallFiles := countVisits(t, filepath.Join(small, "api", "models", Local))
	_, largeFiles := countVisits(t, filepath.Join(large, "api", "models", Local))
	if smallFiles != largeFiles {
		t.Fatalf("hosted scan cost changed with remote cache size: %d vs %d", smallFiles, largeFiles)
	}

	// Meanwhile the full walk grows with the cache, which is the old behaviour.
	_, smallFull := countVisits(t, filepath.Join(small, "api"))
	_, largeFull := countVisits(t, filepath.Join(large, "api"))
	if largeFull <= smallFull {
		t.Fatalf("full walk did not grow with cache size: %d vs %d", smallFull, largeFull)
	}
	t.Logf("full walk files: small=%d large=%d; hosted scan files: %d", smallFull, largeFull, smallFiles)
}