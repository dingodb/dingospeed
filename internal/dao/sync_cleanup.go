package dao

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
)

var syncManifestPins = struct {
	sync.Mutex
	refs map[string]int
}{refs: map[string]int{}}

func syncPinKey(repoType, id, sha string) string {
	return config.SysConfig.Repos() + "\x00" + repoType + "\x00" + id + "\x00" + sha
}

// PinSyncManifest protects content already acquired by an active task until its
// publication finishes. Pause releases pins: retention, not an immortal task
// record, protects parked tasks; resume verifies and reacquires missing content.
func PinSyncManifest(k repository.RepoKey, files []LocalManifestFile) func() {
	key := uploadRepoLockKey(k.RepoType, k.ID())
	uploadRepoLocks.Lock(key)
	keys := make(map[string]struct{}, len(files))
	for _, f := range files {
		keys[syncPinKey(k.RepoType, k.ID(), f.Sha256)] = struct{}{}
	}
	syncManifestPins.Lock()
	for key := range keys {
		syncManifestPins.refs[key]++
	}
	syncManifestPins.Unlock()
	uploadRepoLocks.Unlock(key)
	var once sync.Once
	return func() {
		once.Do(func() {
			syncManifestPins.Lock()
			defer syncManifestPins.Unlock()
			for key := range keys {
				syncManifestPins.refs[key]--
				if syncManifestPins.refs[key] == 0 {
					delete(syncManifestPins.refs, key)
				}
			}
		})
	}
}

func syncManifestPinned(repoType, id, sha string) bool {
	syncManifestPins.Lock()
	defer syncManifestPins.Unlock()
	return syncManifestPins.refs[syncPinKey(repoType, id, sha)] > 0
}

// CleanupSyncPartials never blocks a transfer. It checks mtime after acquiring
// the same per-content gate as DownloadSyncBlob, then removes only old regular
// partials. The budget bounds disk mutations per maintenance tick.
func (u *UploadDao) CleanupSyncPartials(ctx context.Context, retention time.Duration, limit int) (int, error) {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	if limit <= 0 {
		limit = 256
	}
	root := config.SysConfig.Repos()
	dir := filepath.Join(root, ".repository-sync", "content")
	if err := repository.SafePath(root, dir); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-retention)
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if removed >= limit {
			break
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".partial") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".partial")
		if len(name) != 64 || !isHexSHA(name) {
			continue
		}
		value, _ := syncBlobTransfers.LoadOrStore(name, make(chan struct{}, 1))
		gate := value.(chan struct{})
		select {
		case gate <- struct{}{}:
		default:
			continue
		}
		func() {
			defer func() { <-gate }()
			path := filepath.Join(dir, entry.Name())
			info, e := os.Lstat(path)
			if os.IsNotExist(e) {
				return
			}
			if e != nil {
				err = e
				return
			}
			if !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
				return
			}
			if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
				err = e
				return
			}
			removed++
		}()
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func isHexSHA(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// cleanupHostedBatch enumerates registry metadata once but visits only a small
// set of repositories per tick. No shared inventory lock or blob body scan.
func (u *UploadDao) cleanupHostedBatch(ctx context.Context, retention time.Duration, after string) (string, error) {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	root := config.SysConfig.Repos()
	all, err := repository.ListHosted(root)
	if err != nil {
		return after, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].RepoType+"/"+all[i].ID() < all[j].RepoType+"/"+all[j].ID() })
	processed := 0
	last := ""
	for _, d := range all {
		identity := d.RepoType + "/" + d.ID()
		if d.Source != "hosted" || !d.Persistent || identity <= after {
			continue
		}
		if err := ctx.Err(); err != nil {
			return last, err
		}
		if processed >= 8 {
			return last, nil
		}
		processed++
		last = identity
		if _, err := u.cleanupRepoSupersededSnapshots(d.RepoType, d.ID(), time.Now().Add(-config.SysConfig.GetUploadSupersededRetention()).Unix()); err != nil {
			return last, err
		}
		dir := filepath.Join(d.FilesRoot(root), "blobs")
		if err := repository.SafePath(root, dir); err != nil {
			return last, err
		}
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return last, err
		}
		blobs := []string{}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasSuffix(entry.Name(), localUploadStageSuffix) {
				continue
			}
			if !entry.Type().IsRegular() {
				return last, fmt.Errorf("unsafe hosted blob entry")
			}
			if err := repository.Segment(entry.Name()); err != nil {
				return last, err
			}
			blobs = append(blobs, entry.Name())
		}
		if _, err = u.cleanupRepoBlobs(root, d.RepoType, d.ID(), blobs, time.Now().Add(-retention)); err != nil {
			return last, err
		}
	}
	return "", nil
}
