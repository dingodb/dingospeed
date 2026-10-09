package service

import (
	"context"
	"dingospeed/pkg/config"
	"dingospeed/pkg/util"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"
)

func initializeSyncStatus(st SyncStatus, r SyncRequest) SyncStatus {
	st.ProtocolVersion = 1
	st.TotalBytes = syncTotalBytes(r)
	st.RepoType = r.RepoType
	st.Namespace = r.Namespace
	st.Repo = r.Repo
	st.Revision = r.Revision
	st.BaseCommit = r.BaseCommit
	st.ExpectedAbsent = r.ExpectedAbsent
	if st.Generation == 0 {
		st.Generation = 1
	}
	if st.SnapshotVersion == 0 {
		st.SnapshotVersion = 1
	}
	return st
}
func decorateSyncStatus(st SyncStatus) SyncStatus {
	// Earlier task receipts stored context cancellation as a visible failure.
	// Intentional terminal stops are expressed by state, not by an error.
	if st.State == "paused" || st.State == "cancelled" {
		st.Error = ""
	}
	st.AllowedActions = []string{}
	if st.Dismissed {
		return st
	}
	switch st.State {
	case "prepared":
		st.AllowedActions = []string{"execute", "cancel"}
	case "queued", "downloading", "resuming":
		st.AllowedActions = []string{"pause", "cancel"}
	case "pausing":
		st.AllowedActions = []string{"cancel"}
	case "paused", "interrupted":
		st.AllowedActions = []string{"resume", "cancel"}
	case "failed":
		st.AllowedActions = []string{"retry", "cancel", "dismiss"}
	case "conflict":
		st.AllowedActions = []string{"cancel", "dismiss"}
	case "completed", "cancelled":
		st.AllowedActions = []string{"dismiss"}
	case "result_unconfirmed", "needs_attention":
		st.AllowedActions = []string{"reconcile"}
	}
	return st
}
func (s *RepositorySyncService) stopSync(id, intent string) (SyncStatus, error) {
	if !syncID(id) {
		return SyncStatus{}, syncError(400, "SYNC_INVALID_ARGUMENT", "invalid taskId")
	}
	unlock := s.lockAdmission("task:" + id)
	defer unlock()
	e, err := s.load(id)
	if err != nil {
		return SyncStatus{}, err
	}
	e.persist.Lock()
	defer e.persist.Unlock()
	s.mu.Lock()
	before := e.status
	state := before.State
	if state == "publishing" || state == "result_unconfirmed" || state == "needs_attention" {
		s.mu.Unlock()
		return decorateSyncStatus(before), syncError(409, "SYNC_PUBLISHING", "final result must be settled before changing this task")
	}
	if state == "completed" || state == "cancelled" || (intent == "pausing" && state == "paused") {
		s.mu.Unlock()
		return decorateSyncStatus(before), nil
	}
	if intent == "pausing" && state != "queued" && state != "downloading" && state != "resuming" && state != "pausing" {
		s.mu.Unlock()
		return decorateSyncStatus(before), syncError(409, "SYNC_INVALID_STATE", "task cannot be paused")
	}
	next := before
	next.State = intent
	cancel := e.cancel
	if cancel == nil {
		if intent == "canceling" {
			next.State = "cancelled"
		} else {
			next.State = "paused"
		}
	}
	next.Error = ""
	next.SnapshotVersion++
	next.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
	if err = util.WriteDataToFileAtomic(s.taskPath(id, "status"), next); err != nil {
		return decorateSyncStatus(before), err
	}
	s.mu.Lock()
	e.status = next
	e.savedVersion = next.SnapshotVersion
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return decorateSyncStatus(next), nil
}
func (s *RepositorySyncService) Resume(id string) (SyncStatus, error) {
	return s.restartSync(id, false)
}
func (s *RepositorySyncService) Retry(id string) (SyncStatus, error) { return s.restartSync(id, true) }
func (s *RepositorySyncService) restartSync(id string, retry bool, prepared ...bool) (SyncStatus, error) {
	if !config.SysConfig.Registration().Enabled {
		return SyncStatus{}, syncError(409, "SYNC_STANDALONE", "node registration is disabled")
	}
	if !syncID(id) {
		return SyncStatus{}, syncError(400, "SYNC_INVALID_ARGUMENT", "invalid taskId")
	}
	unlock := s.lockAdmission("task:" + id)
	defer unlock()
	previous, err := s.load(id)
	if err != nil {
		return SyncStatus{}, err
	}
	targetUnlock := s.lockAdmission("target:" + syncTarget(previous.request))
	defer targetUnlock()
	s.mu.Lock()
	st := previous.status
	isPrepare := len(prepared) > 0 && prepared[0]
	if isPrepare && st.State != "prepared" {
		s.mu.Unlock()
		return s.durableSnapshot(previous)
	}
	if previous.cancel != nil || s.active[syncTarget(previous.request)] != "" {
		s.mu.Unlock()
		return decorateSyncStatus(st), syncError(409, "SYNC_REVISION_BUSY", "task or target is still executing")
	}
	if isPrepare && !st.ExpiresAt.IsZero() && time.Now().After(st.ExpiresAt) {
		s.mu.Unlock()
		return decorateSyncStatus(st), syncError(409, "SYNC_PREVIEW_EXPIRED", "prepare a new preview")
	}
	allowed := (isPrepare && st.State == "prepared") || (!retry && !isPrepare && (st.State == "paused" || st.State == "interrupted")) || (retry && st.State == "failed")
	if !allowed || st.Dismissed {
		s.mu.Unlock()
		return decorateSyncStatus(st), syncError(409, "SYNC_INVALID_STATE", "task cannot continue without resolving its current state")
	}
	s.mu.Unlock()
	commit, _, readErr := s.readSyncBaseline(previous.request)
	changed := (!previous.request.ExpectedAbsent && (readErr != nil || commit != previous.request.BaseCommit)) || (previous.request.ExpectedAbsent && readErr == nil)
	if previous.request.ExpectedAbsent && readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return decorateSyncStatus(st), readErr
	}
	if changed {
		_ = s.transition(previous, "conflict", syncError(409, "REVISION_CHANGED", "target changed while the task was stopped"))
		return s.snapshot(previous), syncError(409, "REVISION_CHANGED", "target changed while the task was stopped")
	}
	// The manifest and original publication precondition stay immutable. The
	// existing final conditional PublishTree rejects any intervening target edit.
	st.State = "resuming"
	st.Error = ""
	st.CompletedFiles = 0
	st.Generation++
	st.SnapshotVersion++
	st.UpdatedAt = time.Now().UTC()
	e := &syncEntry{request: previous.request, fingerprint: previous.fingerprint, executionID: previous.executionID, status: st, savedVersion: st.SnapshotVersion}
	if err = util.WriteDataToFileAtomic(s.taskPath(id, "status"), st); err != nil {
		return decorateSyncStatus(previous.status), err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	s.mu.Lock()
	s.entries[id] = e
	s.active[syncTarget(e.request)] = id
	s.mu.Unlock()
	go s.execute(ctx, e)
	return decorateSyncStatus(st), nil
}
func (s *RepositorySyncService) Dismiss(id string) (SyncStatus, error) {
	if !syncID(id) {
		return SyncStatus{}, syncError(400, "SYNC_INVALID_ARGUMENT", "invalid taskId")
	}
	unlock := s.lockAdmission("task:" + id)
	defer unlock()
	e, err := s.load(id)
	if err != nil {
		return SyncStatus{}, err
	}
	e.persist.Lock()
	defer e.persist.Unlock()
	s.mu.Lock()
	st := e.status
	if e.cancel != nil || (st.State != "completed" && st.State != "cancelled" && st.State != "failed" && st.State != "conflict") {
		s.mu.Unlock()
		return decorateSyncStatus(st), syncError(409, "SYNC_ACTIVE", "wait for a settled terminal result before hiding its record")
	}
	s.mu.Unlock()
	if st.Dismissed {
		return decorateSyncStatus(st), nil
	}
	st.Dismissed = true
	st.SnapshotVersion++
	st.UpdatedAt = time.Now().UTC()
	if err = util.WriteDataToFileAtomic(s.taskPath(id, "status"), st); err != nil {
		return SyncStatus{}, err
	}
	s.mu.Lock()
	e.status = st
	e.savedVersion = st.SnapshotVersion
	s.mu.Unlock()
	return decorateSyncStatus(st), nil
}

type SyncList struct {
	Items      []SyncStatus `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

// List reads task summaries only, never manifests, repository trees or payloads.
func (s *RepositorySyncService) List(limit int, after string) (SyncList, error) {
	out := SyncList{Items: []SyncStatus{}}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if after != "" && !syncID(after) {
		return out, syncError(400, "SYNC_INVALID_ARGUMENT", "invalid cursor")
	}
	dirs, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name() > dirs[j].Name() })
	for _, dir := range dirs {
		id := dir.Name()
		if !dir.IsDir() || !syncID(id) || (after != "" && id >= after) {
			continue
		}
		s.mu.Lock()
		e := s.entries[id]
		var st SyncStatus
		if e != nil {
			st = e.status
		}
		s.mu.Unlock()
		if e == nil {
			b, err := os.ReadFile(s.taskPath(id, "status"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return out, err
			}
			if err = json.Unmarshal(b, &st); err != nil {
				return out, err
			}
			// Legacy or interrupted receipts require one local migration/recovery
			// load. Normal list polling never reads the fixed manifest.
			if st.ProtocolVersion < 1 || syncActive(st.State) {
				var loadErr error
				st, loadErr = s.Status(id)
				if loadErr != nil {
					return out, loadErr
				}
			}
		}
		if e != nil {
			var readErr error
			st, readErr = s.durableSnapshot(e)
			if readErr != nil {
				return out, readErr
			}
		}
		if st.Dismissed {
			continue
		}
		if len(out.Items) == limit {
			out.NextCursor = out.Items[len(out.Items)-1].TaskID
			break
		}
		out.Items = append(out.Items, decorateSyncStatus(st))
	}
	return out, nil
}

// Persist any progress snapshot before exposing its version. Otherwise a crash
// could make clients reject the lower durable version indefinitely. Each read
// touches only this task's small receipt and skips unchanged snapshots.
func (s *RepositorySyncService) durableSnapshot(e *syncEntry) (SyncStatus, error) {
	e.persist.Lock()
	defer e.persist.Unlock()
	s.mu.Lock()
	st := e.status
	saved := e.savedVersion
	s.mu.Unlock()
	if saved != st.SnapshotVersion {
		if err := util.WriteDataToFileAtomic(s.taskPath(e.request.TaskID, "status"), st); err != nil {
			return SyncStatus{}, err
		}
		e.savedVersion = st.SnapshotVersion
	}
	return decorateSyncStatus(st), nil
}

func (s *RepositorySyncService) Execute(id string) (SyncStatus, error) {
	st, err := s.Status(id)
	if err != nil {
		return st, err
	}
	if st.State != "prepared" {
		return st, nil
	}
	return s.restartSync(id, false, true)
}
