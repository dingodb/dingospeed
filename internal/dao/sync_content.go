package dao

import (
	"context"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"fmt"
	"io"
	"os"
)

// CopySyncContent only serves uploaded content referenced by an active local
// revision. Staged blobs and superseded snapshots alone are not public holders.
// Metadata lookup is scoped to this repository; no inventory/global scan occurs.
func (u *UploadDao) CopySyncContent(k repository.RepoKey, file LocalManifestFile, dst io.Writer) error {
	return u.CopySyncContentRange(context.Background(), k, file, 0, file.Size, dst)
}

func (u *UploadDao) CopySyncContentRange(ctx context.Context, k repository.RepoKey, file LocalManifestFile, start, end int64, dst io.Writer) error {
	if start < 0 || end < start || end > file.Size {
		return fmt.Errorf("invalid content range")
	}
	d, err := repository.Read(config.SysConfig.Repos(), k)
	if err != nil || d.Source != "hosted" {
		return fmt.Errorf("uploaded repository unavailable")
	}
	idx := &repoIndex{RepoType: k.RepoType, OrgRepo: k.ID(), TagsOf: make(map[string][]string)}
	collectRepoTags(idx)
	for commit := range idx.TagsOf {
		files, err := u.fileDao.ReadLocalManifest(k.RepoType, k.ID(), commit)
		if err != nil {
			continue
		}
		for _, candidate := range files {
			if candidate == file {
				key := uploadBlobLockKey(k.RepoType, k.ID(), file.Sha256)
				uploadBlobLocks.RLock(key)
				defer uploadBlobLocks.RUnlock(key)
				path := BlobPath(k.RepoType, k.ID(), file.Sha256)
				if err := ensureLocalUploadPathSafe(config.SysConfig.Repos(), path); err != nil {
					return err
				}
				complete, err := syncCacheComplete(path, file.Size)
				if err != nil {
					return err
				}
				if !complete {
					return fmt.Errorf("content cache incomplete")
				}
				input, err := os.Open(path)
				if err != nil {
					return err
				}
				defer input.Close()
				header := &downloader.DingCacheHeader{}
				if err = header.Read(input); err != nil {
					return err
				}
				_, err = io.CopyN(dst, syncContextReader{ctx, io.NewSectionReader(input, header.GetHeaderSize()+start, end-start)}, end-start)
				return err
			}
		}
	}
	return fmt.Errorf("no active revision references requested content")
}
