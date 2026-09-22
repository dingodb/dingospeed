package hfprojection

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func write(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0444); err != nil {
		t.Fatal(err)
	}
}
func response(t *testing.T, p string, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"status_code": 200, "content": hex.EncodeToString(body)})
	write(t, p, b)
}
func blob(t *testing.T, p string, size uint64, mask byte) {
	t.Helper()
	b := make([]byte, 37+size)
	copy(b, "OLAH")
	binary.LittleEndian.PutUint64(b[4:], 8)
	binary.LittleEndian.PutUint64(b[12:], 4)
	binary.LittleEndian.PutUint64(b[20:], size)
	binary.LittleEndian.PutUint64(b[28:], 8)
	b[36] = mask
	copy(b[37:], "abcdefgh")
	write(t, p, b)
}
func fixture(t *testing.T) (Reader, string) {
	t.Helper()
	root := t.TempDir()
	api := filepath.Join(root, "api", "models", "Qwen", "demo")
	response(t, filepath.Join(api, "revision", "main", "meta_get.json"), map[string]any{"sha": "commit1", "siblings": []any{map[string]any{"rfilename": "README.md"}, map[string]any{"rfilename": "weights.bin"}, map[string]any{"rfilename": "missing.bin"}}})
	for _, item := range []struct {
		path, oid string
		size      int
	}{{"README.md", "readme", 8}, {"weights.bin", "weight", 8}, {"missing.bin", "missing", 9}} {
		response(t, filepath.Join(api, "paths-info", "commit1", item.path, "paths-info_post.json"), []any{map[string]any{"path": item.path, "oid": item.oid, "size": item.size}})
	}
	blob(t, filepath.Join(root, "files", "models", "Qwen", "demo", "blobs", "readme"), 8, 3)
	blob(t, filepath.Join(root, "files", "models", "Qwen", "demo", "blobs", "weight"), 8, 1)
	return Reader{Root: root}, api
}

type stamp struct {
	Sum  [32]byte
	Size int64
	Time int64
	Mode os.FileMode
}

func snapshot(t *testing.T, root string) map[string]stamp {
	t.Helper()
	out := map[string]stamp{}
	err := filepath.Walk(root, func(p string, i os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		b := []byte{}
		if !i.IsDir() {
			b, err = os.ReadFile(p)
			if err != nil {
				return err
			}
		}
		out[p] = stamp{sha256.Sum256(b), i.Size(), i.ModTime().UnixNano(), i.Mode()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestLegacyProjectionReadsWithoutChangingAnyCacheEntry(t *testing.T) {
	r, api := fixture(t)
	// A colliding company model must never be discovered as a HF owner.
	response(t, filepath.Join(r.Root, "api", "models", "dingo-local", "datacanvas", "demo", "revision", "main", "meta_get.json"), map[string]any{"sha": "hosted", "siblings": []any{}})
	before := snapshot(t, r.Root)
	for range 2 {
		cat, err := r.Catalog("models")
		if err != nil {
			t.Fatal(err)
		}
		if len(cat.Repos) != 1 || cat.Repos[0].Repo != "Qwen/demo" || cat.Repos[0].Error != "" {
			t.Fatalf("catalog=%+v", cat)
		}
		m, err := r.Manifest("models", "Qwen/demo", "main")
		if err != nil {
			t.Fatal(err)
		}
		if !m.ManifestComplete || !m.MetadataAvailable || m.Commit != "commit1" || len(m.Files) != 3 {
			t.Fatalf("manifest=%+v", m)
		}
		byPath := map[string]File{}
		for _, f := range m.Files {
			byPath[f.Path] = f
		}
		if byPath["README.md"].CacheStatus != "unknown" || byPath["weights.bin"].CacheStatus != "unknown" || byPath["missing.bin"].CacheStatus != "unknown" {
			t.Fatalf("files=%+v", m.Files)
		}
		f, section, err := r.OpenFile("models", "Qwen/demo", "main", "README.md")
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(section)
		f.Close()
		if err != nil || string(b) != "abcdefgh" {
			t.Fatalf("payload=%q err=%v", b, err)
		}
		if _, _, err := r.OpenFile("models", "Qwen/demo", "main", "weights.bin"); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("partial returned %v", err)
		}
		if _, _, err := r.OpenFile("models", "Qwen/demo", "main", "missing.bin"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing returned %v", err)
		}
	}
	if !reflect.DeepEqual(before, snapshot(t, r.Root)) {
		t.Fatal("projection changed cache entries")
	}
	if _, err := os.Stat(filepath.Join(api, "repository.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("projection registered repository")
	}
}

func TestIndexReusesSnapshotRefreshesExplicitlyAndKeepsCommitLookup(t *testing.T) {
	r, api := fixture(t)
	index := NewIndex()
	if _, err := index.Refresh(r.Root, "models"); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"main", "commit1"} {
		manifest, err := index.Manifest(r.Root, "models", "Qwen/demo", revision)
		if err != nil || manifest.Commit != "commit1" || len(manifest.Files) != 3 {
			t.Fatalf("revision %s manifest=%+v err=%v", revision, manifest, err)
		}
	}
	if err := os.Remove(filepath.Join(api, "revision", "main", "meta_get.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Manifest(r.Root, "models", "Qwen/demo", "main"); err != nil {
		t.Fatalf("indexed manifest unexpectedly rescanned local cache: %v", err)
	}
	index.Invalidate(r.Root, "models")
	if _, err := index.Manifest(r.Root, "models", "Qwen/demo", "main"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalidated index did not rebuild local facts: %v", err)
	}
}

func TestMetadataMissingKeepsObservedFilesAndCorruptionIsExplicit(t *testing.T) {
	r, api := fixture(t)
	if err := os.Remove(filepath.Join(api, "revision", "main", "meta_get.json")); err != nil {
		t.Fatal(err)
	}
	m, err := r.Manifest("models", "Qwen/demo", "commit1")
	if err != nil || m.MetadataAvailable || m.ManifestComplete || len(m.Files) != 3 {
		t.Fatalf("manifest=%+v err=%v", m, err)
	}
	if _, err = r.Manifest("models", "Qwen/demo", "nonexistent"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing revision err=%v", err)
	}
	write(t, filepath.Join(api, "revision", "main", "meta_get.json"), []byte("broken"))
	cat, err := r.Catalog("models")
	if err != nil || len(cat.Repos) != 1 || cat.Repos[0].Error == "" {
		t.Fatalf("catalog=%+v err=%v", cat, err)
	}
	if _, err = r.Manifest("models", "Qwen/demo", "main"); err == nil {
		t.Fatal("corrupt metadata silently accepted")
	}
}
func TestCorruptOrTruncatedBlobNeverComplete(t *testing.T) {
	for _, mode := range []string{"truncated-mask", "truncated-body", "huge-mask", "invalid-version"} {
		t.Run(mode, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "blob")
			blob(t, p, 8, 3)
			b, _ := os.ReadFile(p)
			switch mode {
			case "truncated-mask":
				b = b[:36]
			case "truncated-body":
				b = b[:39]
			case "huge-mask":
				binary.LittleEndian.PutUint64(b[28:], ^uint64(0))
			case "invalid-version":
				binary.LittleEndian.PutUint64(b[4:], 99)
			}
			os.Chmod(p, 0644)
			if err := os.WriteFile(p, b, 0444); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			status, _, _, _, err := inspect(f, 8)
			if err != nil || status == "complete" {
				t.Fatalf("status=%s err=%v", status, err)
			}
		})
	}
}

func TestUpstreamSiblingsWithoutObjectDetailsStayVisible(t *testing.T) {
	r, api := fixture(t)
	p := filepath.Join(api, "revision", "main", "meta_get.json")
	if err := os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	response(t, p, map[string]any{"sha": "commit1", "siblings": []any{map[string]any{"rfilename": "not-downloaded.bin"}}})
	m, err := r.Manifest("models", "Qwen/demo", "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m.Files {
		if f.Path == "not-downloaded.bin" {
			if f.OID != "" || f.Size != -1 || f.CacheStatus != "unknown" {
				t.Fatalf("unknown upstream file = %+v", f)
			}
			return
		}
	}
	t.Fatal("upstream sibling was dropped")
}

func TestCachedVisibilityAndBranchCommitDeduplication(t *testing.T) {
	for _, tc := range []struct {
		name    string
		private any
		gated   any
		want    string
	}{
		{"public", false, false, "public"}, {"private", true, false, "restricted"}, {"gated", false, "auto", "restricted"}, {"unknown", nil, nil, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, api := fixture(t)
			p := filepath.Join(api, "revision", "main", "meta_get.json")
			os.Chmod(p, 0644)
			response(t, p, map[string]any{"sha": "commit1", "private": tc.private, "gated": tc.gated, "siblings": []any{}})
			cat, err := r.Catalog("models")
			if err != nil {
				t.Fatal(err)
			}
			if len(cat.Repos) != 1 || cat.Repos[0].Access != tc.want || len(cat.Repos[0].Revisions) != 1 || cat.Repos[0].Revisions[0].Name != "main" {
				t.Fatalf("catalog=%+v", cat)
			}
			m, err := r.Manifest("models", "Qwen/demo", "commit1")
			if err != nil || m.Access != tc.want || len(m.Files) != 3 {
				t.Fatalf("manifest=%+v err=%v", m, err)
			}
		})
	}
}

func TestLayoutNamesAreValidRepoNamesAndBlobsAloneAreNotDiscovery(t *testing.T) {
	r := Reader{Root: t.TempDir()}
	for _, name := range []string{"revision", "paths-info", "resolve", "blobs"} {
		response(t, filepath.Join(r.Root, "api", "models", "acme", name, "revision", "main", "meta_get.json"), map[string]any{"sha": "commit1", "siblings": []any{}})
	}
	blob(t, filepath.Join(r.Root, "files", "models", "temporary", "orphan", "blobs", "unreferenced"), 8, 3)
	cat, err := r.Catalog("models")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Repos) != 4 {
		t.Fatalf("catalog=%+v", cat)
	}
	for _, repo := range cat.Repos {
		if repo.Repo == "acme" || repo.Repo == "temporary/orphan" {
			t.Fatalf("misidentified %s", repo.Repo)
		}
	}
}

func TestHiddenCommitPermissionCannotBeDiscardedByBranchDeduplication(t *testing.T) {
	r, api := fixture(t)
	p := filepath.Join(api, "revision", "main", "meta_get.json")
	os.Chmod(p, 0644)
	response(t, p, map[string]any{"sha": "commit1", "private": false, "gated": false, "siblings": []any{}})
	response(t, filepath.Join(api, "revision", "commit1", "meta_get.json"), map[string]any{"sha": "commit1", "private": true, "gated": false, "siblings": []any{}})
	cat, err := r.Catalog("models")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Repos[0].Revisions) != 1 || cat.Repos[0].Access != "restricted" {
		t.Fatalf("catalog=%+v", cat)
	}
}
func TestProjectionRejectsUnsafeIdentitiesAndEscapingLinks(t *testing.T) {
	r, _ := fixture(t)
	for _, repo := range []string{"../demo", "dingo-local/alice", "modelscope/Qwen", "a/b/c"} {
		if _, err := r.Manifest("models", repo, "main"); err == nil {
			t.Fatalf("accepted %s", repo)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	write(t, outside, []byte("private"))
	p := filepath.Join(r.Root, "files", "models", "Qwen", "demo", "resolve", "commit1", "escape.txt")
	os.MkdirAll(filepath.Dir(p), 0755)
	if err := os.Symlink(outside, p); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifest, err := r.Manifest("models", "Qwen/demo", "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		if file.CacheStatus != "unknown" {
			t.Fatalf("listing claimed payload availability: %+v", file)
		}
	}
	if f, _, err := r.OpenFile("models", "Qwen/demo", "main", "escape.txt"); err == nil {
		f.Close()
		t.Fatal("opened external symlink")
	}
	// A valid resolve link into this repository remains readable. No metadata
	// OID is supplied, so this exercises the resolve link rather than blob lookup.
	response(t, filepath.Join(r.Root, "api", "models", "Qwen", "demo", "paths-info", "commit1", "linked.txt", "paths-info_post.json"), []any{map[string]any{"size": 8}})
	if err := os.Symlink(filepath.Join(r.Root, "files", "models", "Qwen", "demo", "blobs", "readme"), filepath.Join(filepath.Dir(p), "linked.txt")); err != nil {
		t.Fatal(err)
	}
	f, content, err := r.OpenFile("models", "Qwen/demo", "main", "linked.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(content)
	if err != nil || string(b) != "abcdefgh" {
		t.Fatalf("valid link content=%q err=%v", b, err)
	}
}
