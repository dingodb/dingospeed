package inventory

import (
	"errors"
	"testing"
)

func TestDurableReportConfirmationAndResetKeepNewMutations(t *testing.T) {
	root := t.TempDir()
	k := Key{Namespace: "team", RepoType: "models", Repo: "full/name"}
	change := func() {
		t.Helper()
		if err := Run(root, k, "metadata", map[string]string{"commit": "fixed"}, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	change()
	var first *Report
	if err := Update(root, func(s *State) error {
		s.Epoch = "old"
		s.NodeID = "A"
		first = &Report{Version: 2, InstanceID: "A", Epoch: "old", Sequence: s.Sequence, Key: k}
		s.Entries[k.ID()].Report = first
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	change()
	q, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if q.Entries[k.ID()].Report.Digest() != first.Digest() {
		t.Fatal("mutation rewrote fixed report")
	}
	ack := func(p *Report) Ack {
		return Ack{Epoch: p.Epoch, Sequence: p.Sequence, Digest: p.Digest(), Status: "accepted"}
	}
	if err = Confirm(root, first, ack(first)); err != nil {
		t.Fatal(err)
	}
	q, _ = Read(root)
	if q.Entries[k.ID()] == nil || q.Entries[k.ID()].Sequence != 2 {
		t.Fatal("old ACK erased new mutation")
	}
	base := &Report{Version: 2, InstanceID: "A", Epoch: "new", Baseline: true}
	if err = Update(root, func(s *State) error { s.Baseline = base; s.Cutoff = s.Sequence; s.ReconcileEpoch = "new"; return nil }); err != nil {
		t.Fatal(err)
	}
	change()
	if err = Confirm(root, base, ack(base)); err != nil {
		t.Fatal(err)
	}
	q, _ = Read(root)
	if q.Epoch != "new" || q.Sequence != 1 || q.Entries[k.ID()].Sequence != 1 || q.Entries[k.ID()].Report != nil {
		t.Fatalf("post-baseline mutation lost: %+v", q)
	}
	// A reset with no concurrent changes leaves zero sequence and an empty queue.
	base.Epoch = "next"
	if err = Update(root, func(s *State) error { s.Baseline = base; s.Cutoff = s.Sequence; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = Confirm(root, base, ack(base)); err != nil {
		t.Fatal(err)
	}
	q, _ = Read(root)
	if q.Sequence != 0 || len(q.Entries) != 0 {
		t.Fatal("manual reconciliation did not clear covered queue")
	}
}

func TestFailedOperationRemainsRecoverableOutsideRepository(t *testing.T) {
	root := t.TempDir()
	k := Key{Namespace: "team", RepoType: "models", Repo: "gone"}
	err := Run(root, k, "delete-repository", k, func() error { return errors.New("crash after filesystem change") })
	if err == nil {
		t.Fatal("expected failure")
	}
	q, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if q.Entries[k.ID()].Operation == nil {
		t.Fatal("lost recovery intent")
	}
	if err = Finish(root, k, "delete-repository"); err != nil {
		t.Fatal(err)
	}
	q, _ = Read(root)
	if !q.Entries[k.ID()].Deleted || q.Entries[k.ID()].Operation != nil {
		t.Fatal("deletion tombstone missing")
	}
}
