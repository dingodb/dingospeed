package service

import (
	"context"
	"dingospeed/internal/dao"
	"dingospeed/pkg/repository"
	"io"
)

func (u *UploadService) SyncBlobAvailable(k repository.RepoKey, f dao.LocalManifestFile) (bool, error) {
	if err := validateRepoLocator(k.RepoType, k.Namespace, k.Repo, "main"); err != nil {
		return false, err
	}
	if err := validateManifestList([]dao.LocalManifestFile{f}); err != nil {
		return false, err
	}
	return u.uploadDao.SyncBlobAvailable(k, f)
}
func (u *UploadService) DownloadSyncBlob(ctx context.Context, k repository.RepoKey, f dao.LocalManifestFile, sourceURLs []string, onBytes func(int64)) error {
	if err := validateRepoLocator(k.RepoType, k.Namespace, k.Repo, "main"); err != nil {
		return err
	}
	if err := validateUploadParam(dao.LocalUploadParam{RepoType: k.RepoType, Namespace: k.Namespace, Repo: k.Repo, Revision: "main", FilePath: f.Path, Sha256: f.Sha256, Size: f.Size}); err != nil {
		return err
	}
	return u.uploadDao.DownloadSyncBlob(ctx, k, f, sourceURLs, onBytes)
}

func (u *UploadService) CopySyncContentRange(ctx context.Context, k repository.RepoKey, f dao.LocalManifestFile, start, end int64, dst io.Writer) error {
	if err := validateRepoLocator(k.RepoType, k.Namespace, k.Repo, "main"); err != nil {
		return err
	}
	if err := validateManifestList([]dao.LocalManifestFile{f}); err != nil {
		return err
	}
	return u.uploadDao.CopySyncContentRange(ctx, k, f, start, end, dst)
}
