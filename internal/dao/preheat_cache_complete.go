package dao

import (
	"os"
	"path/filepath"

	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
)

// PreheatCacheComplete checks current local references and blob headers rather
// than trusting a historical successful job after cache cleanup.
func PreheatCacheComplete(key repository.RepoKey, snapshot *CommitHfSha) bool {
	if key.Validate() != nil || snapshot == nil || repository.Segment(snapshot.Sha) != nil {
		return false
	}
	var total int64
	for _, file := range snapshot.Siblings {
		if repository.Relative(file.Rfilename) != nil {
			return false
		}
		path := ResolvePath(key.RepoType, key.ID(), snapshot.Sha, file.Rfilename)
		path, ok := preheatContentPath(key, path)
		if !ok {
			return false
		}
		size, complete := completePreheatFile(path)
		if !complete || size > snapshot.UsedStorage-total {
			return false
		}
		total += size
	}
	return total == snapshot.UsedStorage
}

// Resolve references may be regular files (copy/hard-link fallback) or links
// directly into this repository's blobs. Parent directories and blob targets
// must still pass the usual no-symlink checks.
func preheatContentPath(key repository.RepoKey, path string) (string, bool) {
	root := config.SysConfig.Repos()
	if repository.SafePath(root, filepath.Dir(path)) != nil {
		return "", false
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		target, err = filepath.Abs(target)
		if err != nil {
			return "", false
		}
		blobs, err := filepath.Abs(filepath.Join(key.FilesRoot(root), "blobs"))
		if err != nil || filepath.Dir(target) != blobs {
			return "", false
		}
		path = target
	}
	if repository.SafePath(root, path) != nil {
		return "", false
	}
	return path, true
}

func completePreheatFile(path string) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	if info.Size() == 0 {
		return 0, true
	} // Empty remote files have no cache header.
	header := &downloader.DingCacheHeader{}
	if header.Read(f) != nil || header.FileSize > uint64(info.Size()) || header.GetHeaderSize() > info.Size()-int64(header.FileSize) {
		return 0, false
	}
	for i := uint64(0); i < header.BlockNumber; i++ {
		present, err := header.BlockMask.Test(i)
		if err != nil || !present {
			return 0, false
		}
	}
	return int64(header.FileSize), true
}
