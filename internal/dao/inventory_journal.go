package dao

import (
	"dingospeed/pkg/config"
	"dingospeed/pkg/inventory"
	"dingospeed/pkg/repository"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type metadataIntent struct {
	Revision string
	Commit   string
	Files    []LocalManifestFile
}
type snapshotEdit struct {
	OldCommit string
	Tags      []string
	Files     []LocalManifestFile
}
type deletionIntent struct {
	Edits   []snapshotEdit
	Recycle []RecycleEntry
}

func inventoryKey(k repository.RepoKey) inventory.Key {
	return inventory.Key{Namespace: k.Namespace, RepoType: k.RepoType, Repo: k.Repo}
}

// Inventory reads bypass serving caches, including during explicit disk audits.
func ReadInventorySnapshot(k repository.RepoKey, revision string) (string, []LocalManifestFile, error) {
	b, err := readCacheContent(filepath.Join(k.Revision(config.SysConfig.Repos(), revision), "meta_get.json"))
	if err != nil {
		return "", nil, err
	}
	var meta struct {
		Sha string `json:"sha"`
	}
	if err = json.Unmarshal(b, &meta); err != nil {
		return "", nil, err
	}
	if len(meta.Sha) != 64 {
		return "", nil, fmt.Errorf("invalid revision pointer")
	}
	f, err := readManifestFile(LocalManifestPath(k.RepoType, k.ID(), meta.Sha))
	return meta.Sha, f, err
}

func (u *UploadDao) writeEffectiveMetadata(repoType, id, revision, commit string, files []LocalManifestFile) error {
	k := RepositoryKey(repoType, id)
	if err := u.recoverInventoryLocked(k); err != nil {
		return err
	}
	if err := RegisterHosted(k.RepoType, k.Namespace, k.Repo); err != nil {
		return err
	}
	p := metadataIntent{revision, commit, files}
	return inventory.Run(config.SysConfig.Repos(), inventoryKey(k), "metadata", p, func() error { return u.writeEffectiveMetadataRaw(repoType, id, revision, commit, files) })
}

// Recovery replays only journaled repositories, under the same locks as normal writes.
func (u *UploadDao) RecoverInventory(k repository.RepoKey) error {
	if err := k.Validate(); err != nil {
		return err
	}
	lock := uploadRepoLockKey(k.RepoType, k.ID())
	repositoryLifecycle.Lock(lock)
	defer repositoryLifecycle.Unlock(lock)
	uploadRepoLocks.Lock(lock)
	defer uploadRepoLocks.Unlock(lock)
	return u.recoverInventoryLocked(k)
}
func (u *UploadDao) recoverInventoryLocked(k repository.RepoKey) error {
	root := config.SysConfig.Repos()
	s, err := inventory.Read(root)
	if err != nil {
		return err
	}
	e := s.Entries[inventoryKey(k).ID()]
	if e == nil || e.Operation == nil {
		return nil
	}
	inventory.Barrier.RLock()
	defer inventory.Barrier.RUnlock()
	switch e.Operation.Kind {
	case "metadata":
		var p metadataIntent
		if err = json.Unmarshal(e.Operation.Data, &p); err != nil {
			return err
		}
		if err = verifyManifestContent(k.RepoType, k.ID(), p.Files); err != nil {
			return err
		}
		err = u.writeEffectiveMetadataRaw(k.RepoType, k.ID(), p.Revision, p.Commit, p.Files)
	case "delete-files":
		var p deletionIntent
		if err = json.Unmarshal(e.Operation.Data, &p); err != nil {
			return err
		}
		err = NewCacheAdminDao(u.fileDao).applyInventoryDeletion(k, p)
	case "delete-repository":
		err = u.removeEmptyRepository(k)
	default:
		return fmt.Errorf("unknown inventory operation %q", e.Operation.Kind)
	}
	if err != nil {
		return err
	}
	return inventory.Finish(root, inventoryKey(k), e.Operation.Kind)
}

// Capture desired manifests before any deletion; replay never depends on the old
// manifests still existing after a crash halfway through a multi-version delete.
func (d *CacheAdminDao) journalSoftDelete(idx *repoIndex, items []DeleteItem) []*DeleteResult {
	k := RepositoryKey(idx.RepoType, idx.OrgRepo)
	u := NewUploadDao(d.fileDao, nil)
	if err := u.recoverInventoryLocked(k); err != nil {
		return failAll(items, err.Error())
	}
	idx = buildRepoIndex(idx.RepoType, idx.OrgRepo)
	wanted := map[string]bool{}
	results := make([]*DeleteResult, 0, len(items))
	p := deletionIntent{}
	for _, item := range items {
		r := &DeleteResult{DeleteItem: item, Status: "skipped", Reason: "no reference found"}
		if item.Path == "" {
			r.Status = "failed"
			r.Reason = "path is required"
		} else {
			for _, ref := range idx.BySha[item.Sha] {
				if ref.Path == item.Path {
					r.Status = "deleted"
					r.Reason = ""
					wanted[item.Path+"\x00"+item.Sha] = true
				}
			}
		}
		results = append(results, r)
	}
	commits := map[string]bool{}
	for _, refs := range idx.BySha {
		for _, ref := range refs {
			commits[ref.Commit] = true
		}
	}
	for commit := range commits {
		files, err := readManifestFile(LocalManifestPath(idx.RepoType, idx.OrgRepo, commit))
		if err != nil {
			return failAll(items, err.Error())
		}
		kept := make([]LocalManifestFile, 0, len(files))
		for _, f := range files {
			if !wanted[f.Path+"\x00"+f.Sha256] {
				kept = append(kept, f)
			}
		}
		if len(kept) != len(files) {
			p.Edits = append(p.Edits, snapshotEdit{commit, idx.TagsOf[commit], kept})
		}
	}
	for sha, refs := range idx.BySha {
		all := true
		paths := []string{}
		tags := []string{}
		for _, ref := range refs {
			if !wanted[ref.Path+"\x00"+sha] {
				all = false
			}
			paths = appendUnique(paths, ref.Path)
			for _, tag := range idx.TagsOf[ref.Commit] {
				tags = appendUnique(tags, tag)
			}
		}
		if all {
			p.Recycle = append(p.Recycle, RecycleEntry{RepoType: k.RepoType, Namespace: k.Namespace, Repo: k.Repo, OrgRepo: k.ID(), Sha: sha, Size: idx.Blobs[sha].Size, Source: CacheSourceUpload, Paths: paths, Revisions: tags, UnlinkedAt: time.Now().Unix()})
		}
	}
	if len(p.Edits) == 0 {
		return results
	}
	err := inventory.Run(config.SysConfig.Repos(), inventoryKey(k), "delete-files", p, func() error { return d.applyInventoryDeletion(k, p) })
	if err != nil {
		return failAll(items, err.Error())
	}
	return results
}
func (d *CacheAdminDao) applyInventoryDeletion(k repository.RepoKey, p deletionIntent) error {
	idx := &repoIndex{RepoType: k.RepoType, OrgRepo: k.ID(), Source: CacheSourceUpload, TagsOf: map[string][]string{}, BySha: map[string][]repoRef{}}
	for _, edit := range p.Edits {
		idx.TagsOf[edit.OldCommit] = edit.Tags
	}
	// Write every target before deleting any old snapshot: a target can also be
	// another edit's old commit. Never remove a manifest a final tag still needs.
	finals := map[string]bool{}
	u := NewUploadDao(d.fileDao, nil)
	for _, edit := range p.Edits {
		commit, err := manifestCommit(edit.Files)
		if err != nil {
			return err
		}
		if len(edit.Files) == 0 && len(edit.Tags) == 0 {
			continue
		}
		finals[commit] = true
		if err = u.writeEffectiveMetadataRaw(k.RepoType, k.ID(), commit, commit, edit.Files); err != nil {
			return err
		}
		for _, tag := range edit.Tags {
			if err = u.writeMeta(k.RepoType, k.ID(), tag, commit, edit.Files); err != nil {
				return err
			}
			if err = inventory.SyncParents(config.SysConfig.Repos(), filepath.Join(k.Revision(config.SysConfig.Repos(), tag), "meta_get.json")); err != nil {
				return err
			}
		}
	}
	for _, edit := range p.Edits {
		if !finals[edit.OldCommit] {
			if err := d.dropSnapshot(idx, edit.OldCommit); err != nil {
				return err
			}
		}
	}
	for _, entry := range p.Recycle {
		if err := writeRecycleEntry(entry); err != nil {
			return err
		}
		if err := inventory.SyncParents(config.SysConfig.Repos(), recycleEntryPath(k.RepoType, k.ID(), entry.Sha)); err != nil {
			return err
		}
	}
	return nil
}

func (u *UploadDao) removeEmptyRepository(k repository.RepoKey) error {
	if err := k.Validate(); err != nil {
		return err
	}
	root := config.SysConfig.Repos()
	for _, p := range []string{k.FilesRoot(root), k.APIRoot(root)} {
		if err := repository.SafePath(root, p); err != nil {
			return err
		}
	}
	// The intent is recorded only after live references and active writers have
	// been excluded. Replays cannot race a new same-name repository.
	if err := os.RemoveAll(k.FilesRoot(root)); err != nil {
		return err
	}
	if err := os.RemoveAll(k.APIRoot(root)); err != nil {
		return err
	}
	if err := inventory.SyncParents(root, k.FilesRoot(root)); err != nil {
		return err
	}
	return inventory.SyncParents(root, k.APIRoot(root))
}
