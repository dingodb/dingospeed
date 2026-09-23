package dao

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dingospeed/pkg/config"
	"dingospeed/pkg/inventory"
	"dingospeed/pkg/repository"
	"go.uber.org/zap"
)

// Ordinary uploads share this lock; deletion waits for active requests to finish.
// Keep this outside repo/revision/blob locks to preserve their lock ordering.
var repositoryLifecycle = newKeyedRWMutex()

func holdRepository(repoType, namespace, repo string) func() {
	key := uploadRepoLockKey(repoType, namespace+"/"+repo)
	repositoryLifecycle.RLock(key)
	return func() { repositoryLifecycle.RUnlock(key) }
}

type RepositoryDeleteResult struct {
	Deleted bool `json:"deleted"`
}

// DeleteHostedRepository completes the existing unlink/purge flow. It refuses
// live references, removes remaining staged/orphan data through the existing
// physical purge primitive, then removes metadata and wakes inventory reporting.
func (u *UploadDao) DeleteHostedRepository(k repository.RepoKey) (*RepositoryDeleteResult, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	if k.Namespace == repository.HuggingFace || k.Namespace == repository.ModelScope {
		return nil, localUploadError{status: 400, code: "HOSTED_REPOSITORY_REQUIRED", msg: "remote caches cannot be deleted as hosted repositories"}
	}
	lock := uploadRepoLockKey(k.RepoType, k.ID())
	repositoryLifecycle.Lock(lock)
	defer repositoryLifecycle.Unlock(lock)
	uploadRepoLocks.Lock(lock)
	defer uploadRepoLocks.Unlock(lock)
	root := config.SysConfig.Repos()
	if err := u.recoverInventoryLocked(k); err != nil {
		return nil, err
	}
	d, err := repository.Read(root, k)
	if os.IsNotExist(err) {
		return &RepositoryDeleteResult{}, nil
	}
	if err != nil {
		return nil, err
	}
	if d.Source != "hosted" {
		return nil, fmt.Errorf("repository is not hosted")
	}
	for _, target := range []string{k.FilesRoot(root), k.APIRoot(root)} {
		if err = repository.SafePath(root, target); err != nil {
			return nil, err
		}
	}
	if err = validateRepositoryReferences(k.RepoType, k.ID()); err != nil {
		return nil, err
	}
	if len(buildRepoIndex(k.RepoType, k.ID()).BySha) != 0 {
		return nil, localUploadError{status: 409, code: "REPOSITORY_NOT_EMPTY", msg: "repository still has file references; retry file deletion first"}
	}
	blobs, err := os.ReadDir(filepath.Join(k.FilesRoot(root), "blobs"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, blob := range blobs {
		if blob.IsDir() {
			return nil, fmt.Errorf("unexpected directory in repository blobs")
		}
		if err = reclaimBlobFile(k.RepoType, k.ID(), blob.Name()); err != nil {
			return nil, err
		}
	}
	if err = inventory.Run(root, inventoryKey(k), "delete-repository", k, func() error { return u.removeEmptyRepository(k) }); err != nil {
		return nil, err
	}
	if u.fileDao.baseData != nil {
		for key := range u.fileDao.baseData.Cache.Items() {
			for _, prefix := range []string{"localManifest/", "meta/", "metadatareq/", "filePathInfo/"} {
				if strings.HasPrefix(key, prefix+k.RepoType+"/"+k.ID()+"/") {
					u.fileDao.baseData.Cache.Delete(key)
				}
			}
		}
	}
	NotifyPublished()
	zap.S().Infof("[REPOSITORY] permanently deleted %s/%s", k.RepoType, k.ID())
	return &RepositoryDeleteResult{Deleted: true}, nil
}
