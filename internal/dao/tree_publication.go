package dao

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"dingospeed/pkg/config"
	"dingospeed/pkg/inventory"
	"dingospeed/pkg/repository"
)

// TreePublicationReceipt records the result of one task-bound publication. It
// survives inventory acknowledgements and later edits to the mutable revision.
type TreePublicationReceipt struct {
	PublicationID  string             `json:"publicationId"`
	RequestDigest  string             `json:"requestDigest"`
	Key            repository.RepoKey `json:"key"`
	Revision       string             `json:"revision"`
	PreviousCommit string             `json:"previousCommit"`
	ExpectedAbsent bool               `json:"expectedAbsent"`
	Commit         string             `json:"commit"`
	State          string             `json:"state"`
}

func publicationHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func treePublication(param LocalPublishTreeParam) (*TreePublicationReceipt, error) {
	if param.PublicationID == "" && param.RequestDigest == "" {
		return nil, nil
	}
	if !publicationHex(param.PublicationID) || !publicationHex(param.RequestDigest) {
		return nil, localUploadError{status: 400, code: "PUBLISH_TREE_INVALID_ARGUMENT", msg: "publicationId and requestDigest must be full SHA256 identifiers"}
	}
	files := append([]LocalManifestFile{}, param.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	commit, err := manifestCommit(files)
	if err != nil {
		return nil, err
	}
	return &TreePublicationReceipt{PublicationID: param.PublicationID, RequestDigest: param.RequestDigest,
		Key:      repository.RepoKey{Namespace: param.Namespace, RepoType: param.RepoType, Repo: param.Repo},
		Revision: param.Revision, PreviousCommit: param.BaseCommit, ExpectedAbsent: param.ExpectedAbsent,
		Commit: commit, State: "prepared"}, nil
}

func publicationPath(id string) string {
	return filepath.Join(config.SysConfig.Repos(), ".upload-inventory", "publications", id+".json")
}

func savePublication(p *TreePublicationReceipt, state string) error {
	cp := *p
	cp.State = state
	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	return inventory.AtomicWrite(publicationPath(p.PublicationID), b)
}

func readPublication(want *TreePublicationReceipt) (*TreePublicationReceipt, error) {
	b, err := os.ReadFile(publicationPath(want.PublicationID))
	if err != nil {
		return nil, err
	}
	var got TreePublicationReceipt
	if err = json.Unmarshal(b, &got); err != nil {
		return nil, err
	}
	state := got.State
	got.State = want.State
	if got != *want {
		return nil, localUploadError{status: 409, code: "PUBLICATION_ID_CONFLICT", msg: "publication identity is bound to another request"}
	}
	got.State = state
	switch state {
	case "prepared", "published", "conflict", "not_published":
	default:
		return nil, fmt.Errorf("invalid publication receipt state")
	}
	return &got, nil
}

// applyMetadataIntent is called under the existing repository lock and journal
// barrier. A task receipt is written before the journal can be cleared. Legacy
// metadata intents retain their existing recovery behavior.
func (u *UploadDao) applyMetadataIntent(k repository.RepoKey, p metadataIntent, recovering bool) error {
	if p.Publication != nil {
		r := p.Publication
		if r.Key != k || r.Revision != p.Revision || r.Commit != p.Commit || !publicationHex(r.PublicationID) || !publicationHex(r.RequestDigest) {
			return fmt.Errorf("invalid task publication journal binding")
		}
		if recovering {
			commit, err := manifestCommit(p.Files)
			if err != nil || commit != p.Commit {
				return fmt.Errorf("publication journal manifest does not match commit")
			}
		}
		// A receipt may have reached disk even if its final fsync/Finish failed.
		// Never downgrade that confirmed result because the alias changed later.
		if receipt, err := readPublication(r); err == nil {
			if receipt.State == "published" || receipt.State == "conflict" {
				return nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		current, err := readRevisionPointer(k, p.Revision)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		allowed := current == r.Commit || (!r.ExpectedAbsent && err == nil && current == r.PreviousCommit) || (r.ExpectedAbsent && errors.Is(err, os.ErrNotExist))
		if !allowed {
			return savePublication(r, "conflict")
		}
	}
	if recovering {
		if err := verifyManifestContent(k.RepoType, k.ID(), p.Files); err != nil {
			return err
		}
	}
	if err := u.writeEffectiveMetadataRaw(k.RepoType, k.ID(), p.Revision, p.Commit, p.Files); err != nil {
		return err
	}
	if p.Publication != nil {
		return savePublication(p.Publication, "published")
	}
	return nil
}

func readRevisionPointer(k repository.RepoKey, revision string) (string, error) {
	b, err := readCacheContent(filepath.Join(k.Revision(config.SysConfig.Repos(), revision), "meta_get.json"))
	if err != nil {
		return "", err
	}
	var p struct {
		Sha string `json:"sha"`
	}
	if err = json.Unmarshal(b, &p); err != nil {
		return "", err
	}
	if !publicationHex(p.Sha) {
		return "", fmt.Errorf("invalid revision pointer")
	}
	return p.Sha, nil
}

// ResolveTreePublication examines only this receipt and this repository's
// pending journal. It never re-downloads or replays an unrelated operation.
// An error means the result remains unknown, not that publication failed.
func (u *UploadDao) ResolveTreePublication(param LocalPublishTreeParam) (*TreePublicationReceipt, error) {
	want, err := treePublication(param)
	if err != nil {
		return nil, err
	}
	if want == nil {
		return nil, fmt.Errorf("publication identity required")
	}
	if err = want.Key.Validate(); err != nil {
		return nil, err
	}
	release := holdRepository(param.RepoType, param.Namespace, param.Repo)
	defer release()
	lock := uploadRepoLockKey(param.RepoType, want.Key.ID())
	uploadRepoLocks.Lock(lock)
	defer uploadRepoLocks.Unlock(lock)
	got, err := readPublication(want)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if got != nil && got.State != "prepared" {
		return got, nil
	}
	q, err := inventory.Read(config.SysConfig.Repos())
	if err != nil {
		return nil, err
	}
	entry := q.Entries[inventoryKey(want.Key).ID()]
	if entry != nil && entry.Operation != nil && entry.Operation.Kind == "metadata" {
		var p metadataIntent
		if err = json.Unmarshal(entry.Operation.Data, &p); err != nil {
			return nil, err
		}
		if p.Publication != nil && p.Publication.PublicationID == want.PublicationID {
			if *p.Publication != *want {
				return nil, fmt.Errorf("publication journal does not match request")
			}
			if err = u.recoverInventoryLocked(want.Key); err != nil {
				return nil, err
			}
			return readPublication(want)
		}
	}
	// No matching intent can apply this prepared receipt later. A coincidental
	// matching commit on disk alone is never evidence of this task's success.
	want.State = "not_published"
	return want, nil
}
