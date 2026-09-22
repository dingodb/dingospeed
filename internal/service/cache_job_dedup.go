package service

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"dingospeed/internal/dao"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
)

// Data stays numeric for existing cacheJob/create clients.
type CacheJobCreateResult struct {
	ID          int64  `json:"data"`
	Msg         string `json:"msg"`
	Disposition string `json:"disposition"`
	Commit      string `json:"commit,omitempty"`
}

// Called with createMu held through submission. Each Speed owns its node's
// worker pool; all control-plane replicas reach this same admission boundary.
func (p *CacheJobService) reusableCacheJob(key repository.RepoKey, snapshot *dao.CommitHfSha, local bool, instanceID string) (*CacheJobStatus, error) {
	entries, err := os.ReadDir(filepath.Join(config.SysConfig.Repos(), "cache-jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var completed *CacheJobStatus
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		status, err := p.CacheJobStatus(id)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if status.RepoKey != key || status.Commit != snapshot.Sha {
			continue
		}
		switch status.State {
		case "running", "pausing", "resuming", "canceling":
			return &status, nil
		case "waiting":
			if _, queued := p.cachePool.GetTask(int(id)); queued {
				return &status, nil
			}
		case "complete":
			if status.Local != local || (!local && status.InstanceID != instanceID) {
				continue
			}
			if completed == nil || status.ID > completed.ID {
				completed = &status
			}
		}
	}
	if completed != nil && dao.PreheatCacheComplete(key, snapshot) {
		return completed, nil
	}
	return nil, nil
}
