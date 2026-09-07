package dao

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"dingospeed/internal/data"
	"dingospeed/pkg/config"
)

// BenchmarkListFilesExactRepo compares the current exact-key path with the old
// call chain, which first discovered every repository using revision as the only
// API marker. The fixture is intentionally small: seven repositories and 500
// empty paths-info entries per repository.
func BenchmarkListFilesExactRepo(b *testing.B) {
	repos := b.TempDir()
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{
		Server: config.ServerConfig{Repos: repos},
		Upload: config.Upload{Namespace: "dingo-local"},
	}
	b.Cleanup(func() { config.SysConfig = oldConfig })

	baseData := data.NewBaseData()
	lockDao := NewLockDao(baseData)
	admin := NewCacheAdminDao(NewFileDao(nil, baseData, lockDao))

	for repo := 0; repo < 7; repo++ {
		orgRepo := "remote/repo-" + string(rune('a'+repo))
		if repo == 0 {
			orgRepo = "dingo-local/target"
		}
		if err := os.MkdirAll(filepath.Join(repoApiRoot("models", orgRepo), "revision", "main"), 0o755); err != nil {
			b.Fatal(err)
		}
		for file := 0; file < 500; file++ {
			path := filepath.Join(repoApiRoot("models", orgRepo), "paths-info", "commit", "dir", strconv.Itoa(file))
			if err := os.MkdirAll(path, 0o755); err != nil {
				b.Fatal(err)
			}
		}
	}

	b.Run("direct", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = admin.ListFiles("models", "dingo-local/target")
		}
	})
	b.Run("legacy-global-discovery", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = legacyListFilesForBenchmark("models", "dingo-local/target")
		}
	})
}

func legacyListFilesForBenchmark(repoType, orgRepo string) []*CacheFileRow {
	keys := make(map[repoKey]struct{})
	root := filepath.Join(config.SysConfig.Repos(), "api", repoType)
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() || path == root {
			return nil
		}
		if entry.Name() != "revision" {
			return nil
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			keys[repoKey{RepoType: repoType, OrgRepo: filepath.ToSlash(rel)}] = struct{}{}
		}
		return fs.SkipDir
	})
	ordered := make([]repoKey, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].OrgRepo < ordered[j].OrgRepo })
	for _, key := range ordered {
		if key.OrgRepo == orgRepo {
			return indexRows(buildRepoIndex(key.RepoType, key.OrgRepo))
		}
	}
	return []*CacheFileRow{}
}
