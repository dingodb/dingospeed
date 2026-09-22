package dao

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
)

func TestPreheatCacheCompleteReferences(t *testing.T) {
	for _, provider := range []string{repository.HuggingFace, repository.ModelScope} {
		for _, mode := range []string{"relative-link", "absolute-link", "regular", "missing", "external", "other-repository", "dangling", "chained", "parent-link", "partial", "truncated", "size-mismatch"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				old := config.SysConfig
				t.Cleanup(func() { config.SysConfig = old })
				root := t.TempDir()
				config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: root}}
				key := repository.RepoKey{Namespace: provider, RepoType: "models", Repo: "owner/model"}
				var snapshot CommitHfSha
				if err := json.Unmarshal([]byte(`{"sha":"commit1","siblings":[{"rfilename":"model.bin"}]}`), &snapshot); err != nil {
					t.Fatal(err)
				}
				snapshot.UsedStorage = 8
				payload := make([]byte, 45)
				copy(payload, "OLAH")
				binary.LittleEndian.PutUint64(payload[4:], 8)
				binary.LittleEndian.PutUint64(payload[12:], 4)
				binary.LittleEndian.PutUint64(payload[20:], 8)
				binary.LittleEndian.PutUint64(payload[28:], 8)
				payload[36] = 3
				copy(payload[37:], "abcdefgh")
				if mode == "partial" {
					payload[36] = 1
				}
				if mode == "truncated" {
					payload = payload[:40]
				}
				if mode == "size-mismatch" {
					snapshot.UsedStorage = 9
				}
				write := func(path string) {
					t.Helper()
					if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, payload, 0600); err != nil {
						t.Fatal(err)
					}
				}
				link := func(target, path string) {
					t.Helper()
					if err := os.Symlink(target, path); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}
				blob := key.Blob(root, "content")
				write(blob)
				path := key.Resolve(root, snapshot.Sha, "model.bin")
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "regular":
					write(path)
				case "missing":
				case "parent-link":
					if err := os.Remove(filepath.Dir(path)); err != nil {
						t.Fatal(err)
					}
					parent := t.TempDir()
					write(filepath.Join(parent, "model.bin"))
					link(parent, filepath.Dir(path))
				default:
					target := blob
					switch mode {
					case "external":
						target = filepath.Join(t.TempDir(), "content")
						write(target)
					case "other-repository":
						other := key
						other.Repo = "owner/other"
						target = other.Blob(root, "content")
						write(target)
					case "dangling":
						target = key.Blob(root, "absent")
					case "chained":
						target = key.Blob(root, "alias")
						link(blob, target)
					case "relative-link":
						var err error
						target, err = filepath.Rel(filepath.Dir(path), blob)
						if err != nil {
							t.Fatal(err)
						}
					}
					link(target, path)
				}
				want := mode == "relative-link" || mode == "absolute-link" || mode == "regular"
				if got := PreheatCacheComplete(key, &snapshot); got != want {
					t.Fatalf("complete=%v want=%v", got, want)
				}
			})
		}
	}
}
