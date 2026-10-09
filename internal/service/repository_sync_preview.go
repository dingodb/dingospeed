package service

import (
	"context"
	"dingospeed/pkg/repository"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Expiration applies only to prepared, never-executed previews. Executed task
// receipts and cached files have independent lifecycles and are never removed.
func (s *RepositorySyncService) expirePrepared(ctx context.Context) {
	s.mu.Lock()
	ids := []string{}
	for id, e := range s.entries {
		if e.cancel == nil && e.status.State == "prepared" && e.status.Generation == 1 && !e.status.ExpiresAt.IsZero() && time.Now().After(e.status.ExpiresAt) {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.expirePreparedID(id)
	}
}
func (s *RepositorySyncService) expirePreparedID(id string) {
	if !syncID(id) {
		return
	}
	unlock := s.lockAdmission("task:" + id)
	defer unlock()
	s.mu.Lock()
	e := s.entries[id]
	if e != nil && (e.cancel != nil || e.status.State != "prepared" || e.status.Generation != 1) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	b, err := os.ReadFile(s.taskPath(id, "status"))
	var st SyncStatus
	if err != nil || json.Unmarshal(b, &st) != nil || st.State != "prepared" || st.Generation != 1 || st.ExpiresAt.IsZero() || !time.Now().After(st.ExpiresAt) {
		return
	}
	dir := filepath.Dir(s.taskPath(id, "status"))
	if repository.SafePath(s.root, dir) != nil {
		return
	}
	// Remove only our two preview records; unknown files keep the directory.
	if err = os.Remove(s.taskPath(id, "manifest")); err != nil && !os.IsNotExist(err) {
		return
	}
	if err = os.Remove(s.taskPath(id, "status")); err != nil && !os.IsNotExist(err) {
		return
	}
	_ = os.Remove(dir)
	s.mu.Lock()
	delete(s.entries, id)
	s.mu.Unlock()
}
