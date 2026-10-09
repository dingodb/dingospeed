package dao

import (
	"errors"
	"os"
	"strings"
	"testing"

	"dingospeed/pkg/inventory"
)

func TestTreePublicationReceiptWriteFailureKeepsRecoverableIntent(t *testing.T) {
	u, root := newTestUploadDao(t)
	base, keep, _ := seedRevision(t, u, "main")
	p := publicationParam("main", base, "6", keep)
	want, _ := treePublication(p)
	if err := savePublication(want, "prepared"); err != nil {
		t.Fatal(err)
	}
	intent := metadataIntent{Revision: p.Revision, Commit: want.Commit, Files: p.Files, Publication: want}
	// Use a directory at the receipt path to force the real atomic
	// rename to fail after metadata writes, without platform permission tricks.
	err := inventory.Run(root, inventoryKey(want.Key), "metadata", intent, func() error {
		if err := u.writeEffectiveMetadataRaw(p.RepoType, want.Key.ID(), p.Revision, want.Commit, p.Files); err != nil {
			return err
		}
		if err := os.Remove(publicationPath(want.PublicationID)); err != nil {
			return err
		}
		if err := os.Mkdir(publicationPath(want.PublicationID), 0700); err != nil {
			return err
		}
		return savePublication(want, "published")
	})
	if err == nil {
		t.Fatal("expected receipt rename failure")
	}
	if _, err = u.ResolveTreePublication(p); err == nil {
		t.Fatal("unreadable receipt must stay uncertain")
	}
	q, _ := inventory.Read(root)
	if q.Entries[inventoryKey(want.Key).ID()].Operation == nil {
		t.Fatal("lost pending operation")
	}
	// Clear the injected I/O fault. The journal still supplies the task binding.
	if err = os.Remove(publicationPath(want.PublicationID)); err != nil {
		t.Fatal(err)
	}
	got, err := u.ResolveTreePublication(p)
	if err != nil || got.State != "published" {
		t.Fatalf("recovery: %+v %v", got, err)
	}
}

func TestTreePublicationPendingFinishDoesNotDowngradePublishedReceipt(t *testing.T) {
	u, root := newTestUploadDao(t)
	base, keep, _ := seedRevision(t, u, "main")
	p := publicationParam("main", base, "7", keep)
	want, _ := treePublication(p)
	if err := savePublication(want, "prepared"); err != nil {
		t.Fatal(err)
	}
	intent := metadataIntent{Revision: p.Revision, Commit: want.Commit, Files: p.Files, Publication: want}
	_ = inventory.Run(root, inventoryKey(want.Key), "metadata", intent, func() error {
		if err := u.applyMetadataIntent(want.Key, intent, false); err != nil {
			return err
		}
		return errors.New("interrupted before Finish")
	})
	newCommit, _ := manifestCommit([]LocalManifestFile{})
	if err := u.writeEffectiveMetadataRaw(p.RepoType, want.Key.ID(), p.Revision, newCommit, nil); err != nil {
		t.Fatal(err)
	}
	if err := u.RecoverInventory(want.Key); err != nil {
		t.Fatal(err)
	}
	got, err := u.ResolveTreePublication(p)
	if err != nil || got.State != "published" {
		t.Fatalf("confirmed result downgraded: %+v %v", got, err)
	}
	if current := revisionCommit(t, u, "models", "dingo-local/demo", "main"); current != newCommit {
		t.Fatal("newer alias overwritten")
	}
}

func publicationParam(revision, base, id string, files ...LocalManifestFile) LocalPublishTreeParam {
	p := treeParam(revision, base, files...)
	p.PublicationID, p.RequestDigest = strings.Repeat(id, 64), strings.Repeat("a", 64)
	return p
}

func TestTreePublicationReceiptSurvivesLaterRevisionAndAck(t *testing.T) {
	u, root := newTestUploadDao(t)
	base, keep, drop := seedRevision(t, u, "main")
	p := publicationParam("main", base, "1", keep)
	r := mustPublishTree(t, u, p)
	want, _ := treePublication(p)
	// An unrelated later edit must neither invalidate the original receipt nor
	// be rolled back when the original request is repeated.
	later := mustPublishTree(t, u, treeParam("main", r.Commit, keep, drop))
	if err := inventory.Update(root, func(q *inventory.State) error { delete(q.Entries, inventoryKey(want.Key).ID()); return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := u.ResolveTreePublication(p)
	if err != nil || got.State != "published" || got.Commit != r.Commit {
		t.Fatalf("receipt: %+v %v", got, err)
	}
	replay := mustPublishTree(t, u, p)
	if replay.Commit != r.Commit {
		t.Fatal("replay lost original result")
	}
	if current := revisionCommit(t, u, "models", "dingo-local/demo", "main"); current != later.Commit {
		t.Fatal("receipt resolution rolled back newer revision")
	}
	commit, files, err := ReadInventorySnapshot(got.Key, got.Commit)
	if err != nil || commit != r.Commit || len(files) != 1 {
		t.Fatalf("immutable readback: %s %+v %v", commit, files, err)
	}
}

func TestTreePublicationRecoveryAfterEffectBeforeReceipt(t *testing.T) {
	u, root := newTestUploadDao(t)
	base, keep, _ := seedRevision(t, u, "main")
	p := publicationParam("main", base, "2", keep)
	want, _ := treePublication(p)
	if err := savePublication(want, "prepared"); err != nil {
		t.Fatal(err)
	}
	intent := metadataIntent{Revision: p.Revision, Commit: want.Commit, Files: p.Files, Publication: want}
	// Fault at the exact post-effect/pre-receipt boundary leaves the existing
	// journal pending, as a failed savePublication would in production.
	err := inventory.Run(root, inventoryKey(want.Key), "metadata", intent, func() error {
		if err := u.writeEffectiveMetadataRaw(p.RepoType, want.Key.ID(), p.Revision, want.Commit, p.Files); err != nil {
			return err
		}
		return errors.New("injected receipt storage failure")
	})
	if err == nil {
		t.Fatal("fault not injected")
	}
	got, err := u.ResolveTreePublication(p)
	if err != nil || got.State != "published" {
		t.Fatalf("recovery: %+v %v", got, err)
	}
	q, _ := inventory.Read(root)
	if q.Entries[inventoryKey(want.Key).ID()].Operation != nil {
		t.Fatal("successful receipt did not finish journal")
	}
}

func TestTreePublicationRecoveryFencesNewerRevision(t *testing.T) {
	u, root := newTestUploadDao(t)
	base, keep, _ := seedRevision(t, u, "main")
	p := publicationParam("main", base, "3", keep)
	want, _ := treePublication(p)
	if err := savePublication(want, "prepared"); err != nil {
		t.Fatal(err)
	}
	intent := metadataIntent{Revision: p.Revision, Commit: want.Commit, Files: p.Files, Publication: want}
	_ = inventory.Run(root, inventoryKey(want.Key), "metadata", intent, func() error { return errors.New("interrupted before effect") })
	// Simulate a newer external mutation without invoking automatic recovery.
	newCommit, _ := manifestCommit([]LocalManifestFile{})
	if err := u.writeEffectiveMetadataRaw(p.RepoType, want.Key.ID(), p.Revision, newCommit, nil); err != nil {
		t.Fatal(err)
	}
	got, err := u.ResolveTreePublication(p)
	if err != nil || got.State != "conflict" {
		t.Fatalf("recovery: %+v %v", got, err)
	}
	if current := revisionCommit(t, u, "models", "dingo-local/demo", "main"); current != newCommit {
		t.Fatal("recovery overwrote concurrent change")
	}
}

func TestTreePublicationPreparedWithoutJournalIsNotPublished(t *testing.T) {
	u, root := newTestUploadDao(t)
	base, keep, _ := seedRevision(t, u, "main")
	p := publicationParam("main", base, "4", keep)
	want, _ := treePublication(p)
	if err := savePublication(want, "prepared"); err != nil {
		t.Fatal(err)
	}
	// The journal refuses the operation before invoking its callback.
	if err := inventory.Update(root, func(q *inventory.State) error { q.Sequence = ^uint64(0); return nil }); err != nil {
		t.Fatal(err)
	}
	called := false
	err := inventory.Run(root, inventoryKey(want.Key), "metadata", metadataIntent{}, func() error { called = true; return nil })
	if err == nil || called {
		t.Fatal("expected failure before effective change")
	}
	got, err := u.ResolveTreePublication(p)
	if err != nil || got.State != "not_published" {
		t.Fatalf("resolution: %+v %v", got, err)
	}
	// Matching content alone is not a task receipt.
	if err = u.writeEffectiveMetadataRaw(p.RepoType, want.Key.ID(), p.Revision, want.Commit, p.Files); err != nil {
		t.Fatal(err)
	}
	got, err = u.ResolveTreePublication(p)
	if err != nil || got.State != "not_published" {
		t.Fatalf("coincidental commit accepted: %+v %v", got, err)
	}
}

func TestTreePublicationNoopReceiptAndRequestBinding(t *testing.T) {
	u, _ := newTestUploadDao(t)
	base, keep, drop := seedRevision(t, u, "main")
	p := publicationParam("main", base, "5", keep, drop)
	r := mustPublishTree(t, u, p)
	if r.Changed {
		t.Fatal("no-op changed metadata")
	}
	got, err := u.ResolveTreePublication(p)
	if err != nil || got.State != "published" {
		t.Fatalf("no-op receipt: %+v %v", got, err)
	}
	p.RequestDigest = strings.Repeat("b", 64)
	if _, err = u.ResolveTreePublication(p); err == nil {
		t.Fatal("request digest binding ignored")
	}
}
