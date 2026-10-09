package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"dingospeed/internal/dao"
	"go.uber.org/zap"
)

const maxSyncRecoveryAttempts = 6

func (s *RepositorySyncService) publication(e *syncEntry) dao.LocalPublishTreeParam {
	p := e.request.publication()
	p.PublicationID = s.snapshot(e).PublicationID
	p.RequestDigest = e.fingerprint
	return p
}

func (s *RepositorySyncService) signalRecovery() {
	select {
	case s.wakeRecovery <- struct{}{}:
	default:
	}
}

// RunRecovery is owned by the upload server lifetime. Only task receipts are
// enumerated once at startup; recurring retries visit in-memory pending tasks.
// It never resumes transfers or submits a new PublishTree request.
func (s *RepositorySyncService) RunRecovery(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	scanned := false
	var scanAfter time.Time
	for {
		if !scanned && !time.Now().Before(scanAfter) {
			if err := s.loadPendingReceipts(ctx); err == nil {
				scanned = true
			} else {
				zap.S().Warnf("sync task recovery discovery failed; retrying: %v", err)
				scanAfter = time.Now().Add(30 * time.Second)
			}
		}
		s.recoverDue(ctx)
		s.expirePrepared(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wakeRecovery:
		}
	}
}

func (s *RepositorySyncService) loadPendingReceipts(ctx context.Context) error {
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, dir := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !dir.IsDir() || !syncID(dir.Name()) {
			continue
		}
		// Completed history can contain large manifests. Inspect its small status
		// first; only unfinished tasks need their fixed manifest loaded.
		b, err := os.ReadFile(s.taskPath(dir.Name(), "status"))
		var st SyncStatus
		if err != nil || json.Unmarshal(b, &st) != nil {
			continue
		}
		if st.State == "prepared" && !st.ExpiresAt.IsZero() && time.Now().After(st.ExpiresAt) {
			s.expirePreparedID(dir.Name())
			continue
		}
		if st.ProtocolVersion >= 1 && st.State != "prepared" && !syncActive(st.State) && st.State != "result_unconfirmed" {
			continue
		}
		unlock := s.lockAdmission("task:" + dir.Name())
		_, err = s.load(dir.Name())
		unlock()
		if err != nil {
			zap.S().Warnf("cannot load pending sync task %s: %v", dir.Name(), err)
		}
	}
	return nil
}

func (s *RepositorySyncService) recoverDue(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	ids := make([]string, 0)
	for id, e := range s.entries {
		if e.cancel == nil && e.status.State == "result_unconfirmed" && !e.status.RetryAt.After(now) {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		unlock := s.lockAdmission("task:" + id)
		s.mu.Lock()
		e := s.entries[id]
		eligible := e != nil && e.cancel == nil && e.status.State == "result_unconfirmed" && !e.status.RetryAt.After(time.Now())
		s.mu.Unlock()
		if eligible {
			s.recoverPublication(e)
		}
		unlock()
	}
}

// Reconcile retries result inspection only. It cannot bypass the original
// target condition or restart a cancelled/interrupted download.
func (s *RepositorySyncService) Reconcile(id string) (SyncStatus, error) {
	if !syncID(id) {
		return SyncStatus{}, syncError(400, "SYNC_INVALID_ARGUMENT", "invalid taskId")
	}
	unlock := s.lockAdmission("task:" + id)
	defer unlock()
	e, err := s.load(id)
	if err != nil {
		return SyncStatus{}, err
	}
	s.mu.Lock()
	if e.cancel != nil {
		s.mu.Unlock()
		return SyncStatus{}, syncError(409, "SYNC_ACTIVE", "task is still executing")
	}
	state := e.status.State
	if state == "result_unconfirmed" || state == "needs_attention" {
		e.status.RecoveryAttempts = 0
		e.status.RetryAt = time.Time{}
	}
	s.mu.Unlock()
	if state == "result_unconfirmed" || state == "needs_attention" {
		s.recoverPublication(e)
	}
	return s.snapshot(e), nil
}

func (s *RepositorySyncService) recoverPublication(e *syncEntry) {
	if e.request.Remote != nil {
		s.recoverRemotePublication(e)
		return
	}
	if s.snapshot(e).PublicationID == "" {
		// Older releases did not bind publication to the task. Equal content
		// alone is not evidence that this task published it.
		s.setRecoveryResult(e, "needs_attention", fmt.Errorf("legacy task has no publication receipt; inspect the target, no publication was replayed"))
		return
	}
	r, err := s.upload.uploadDao.ResolveTreePublication(s.publication(e))
	if err != nil {
		s.deferRecovery(e, err)
		return
	}
	switch r.State {
	case "published":
		// Read the immutable result, not the mutable alias that another writer
		// may already have advanced. This reads metadata, never payload bytes.
		commit, files, err := dao.ReadInventorySnapshot(e.request.key(), r.Commit)
		if err != nil {
			s.deferRecovery(e, fmt.Errorf("publication succeeded but result metadata is unavailable: %w", err))
			return
		}
		if commit != r.Commit || digestJSON(files) != s.snapshot(e).ManifestDigest {
			s.deferRecovery(e, fmt.Errorf("publication receipt and immutable manifest differ"))
			return
		}
		s.mu.Lock()
		e.status.Commit = r.Commit
		e.status.CompletedFiles = e.status.TotalFiles
		s.mu.Unlock()
		s.setRecoveryResult(e, "completed", nil)
	case "not_published":
		s.setRecoveryResult(e, "interrupted", fmt.Errorf("publication did not take effect; resume using the original target condition"))
	case "conflict":
		s.setRecoveryResult(e, "conflict", fmt.Errorf("target changed; recorded publication was not replayed over it"))
	default:
		s.deferRecovery(e, fmt.Errorf("publication remains unresolved: %s", r.State))
	}
}

func (s *RepositorySyncService) setRecoveryResult(e *syncEntry, state string, cause error) {
	s.mu.Lock()
	e.status.RetryAt = time.Time{}
	s.mu.Unlock()
	if err := s.transition(e, state, cause); err != nil {
		s.deferRecovery(e, fmt.Errorf("saving recovered task result: %w", err))
	}
}

func (s *RepositorySyncService) deferRecovery(e *syncEntry, cause error) {
	s.mu.Lock()
	e.status.RecoveryAttempts++
	attempts := e.status.RecoveryAttempts
	state := "result_unconfirmed"
	if attempts >= maxSyncRecoveryAttempts {
		state = "needs_attention"
		e.status.RetryAt = time.Time{}
	} else {
		e.status.RetryAt = time.Now().Add(time.Second * time.Duration(1<<min(attempts, 5)))
	}
	s.mu.Unlock()
	_ = s.transition(e, state, cause)
}
