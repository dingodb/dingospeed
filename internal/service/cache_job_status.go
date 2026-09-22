package service

import (
	"context"
	"dingospeed/pkg/common"
	"dingospeed/pkg/consts"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dingospeed/internal/service/task"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"go.uber.org/zap"
)

type CacheJobStatus struct {
	ID              int64   `json:"id"`
	ProtocolVersion int     `json:"protocolVersion"`
	Generation      uint64  `json:"generation"`
	StateVersion    uint64  `json:"stateVersion"`
	SnapshotVersion uint64  `json:"snapshotVersion"`
	ActiveSeconds   float64 `json:"activeSeconds"`
	ErrorCode       string  `json:"errorCode,omitempty"`
	repository.RepoKey
	Commit      string    `json:"commit"`
	State       string    `json:"state"`
	CachedBytes uint64    `json:"cachedBytes"`
	TotalBytes  uint64    `json:"totalBytes"`
	Error       string    `json:"error,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Local       bool      `json:"local,omitempty"`
	InstanceID  string    `json:"instanceId,omitempty"`
}

// Reserve an ID on disk so concurrent submissions and restarts cannot replace
// another job. Millisecond IDs fit exactly in the console's JavaScript number.
func reserveLocalCacheJob() (int64, error) {
	for id := time.Now().UnixMilli(); ; id++ {
		path := cacheJobStatusPath(id)
		if err := util.MakeDirs(path); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		err = json.NewEncoder(f).Encode(CacheJobStatus{ID: id, Local: true, State: "interrupted", UpdatedAt: time.Now().UTC()})
		closeErr := f.Close()
		if err != nil {
			return 0, err
		}
		if closeErr != nil {
			return 0, closeErr
		}
		return id, nil
	}
}

func cacheJobStatusPath(id int64) string {
	return filepath.Join(config.SysConfig.Repos(), "cache-jobs", fmt.Sprintf("%d.json", id))
}
func writeCacheJobStatus(status CacheJobStatus) error {
	if status.UpdatedAt.IsZero() {
		status.UpdatedAt = time.Now().UTC()
	}
	path := cacheJobStatusPath(status.ID)
	if err := util.MakeDirs(path); err != nil {
		return err
	}
	return util.WriteDataToFileAtomic(path, status)
}

// Serializes durable snapshots and the corresponding execution ownership.
// Keys include the data root: separate local nodes never share execution identity.
var cacheStateMu sync.Mutex
var cacheExecutions = map[string]*cacheExecution{}

type cacheExecution struct {
	trace       *common.CacheTransferTrace
	state       string
	task        *task.PreheatCacheTask
	generation  uint64
	finished    bool
	startedWork bool
	result      error
	serviceCtx  context.Context
	started     time.Time
	baseSeconds float64
	stopStarted time.Time
	finishedAt  time.Time
}

func readCacheJobStatus(id int64) (CacheJobStatus, error) {
	var status CacheJobStatus
	if id <= 0 {
		return status, fmt.Errorf("invalid job id")
	}
	b, err := os.ReadFile(cacheJobStatusPath(id))
	if err != nil {
		return status, err
	}
	err = json.Unmarshal(b, &status)
	return status, err
}

func commitCacheSnapshot(before CacheJobStatus, next *CacheJobStatus) error {
	next.ProtocolVersion = 1
	next.StateVersion = before.StateVersion
	if next.State != before.State || next.StateVersion == 0 {
		next.StateVersion++
	}
	next.SnapshotVersion = before.SnapshotVersion + 1
	next.UpdatedAt = time.Now().UTC()
	err := writeCacheJobStatus(*next)
	if err == nil {
		if execution := cacheExecutions[cacheJobStatusPath(next.ID)]; execution != nil {
			execution.state = next.State
		}
	}
	if err != nil {
		cachePersistenceFailures.Inc()
	} else if before.State != next.State {
		cacheTransitions.WithLabelValues(next.State).Inc()
	}
	return err
}

func cacheActive(state string) bool {
	switch state {
	case "waiting", "running", "pausing", "resuming", "canceling":
		return true
	}
	return false
}

func (p *CacheJobService) CacheJobStatus(id int64) (CacheJobStatus, error) {
	cacheStateMu.Lock()
	defer cacheStateMu.Unlock()
	return cacheJobSnapshotLocked(id)
}

func cacheJobSnapshotLocked(id int64) (CacheJobStatus, error) {
	status, err := readCacheJobStatus(id)
	if err != nil {
		return status, err
	}
	before := status
	if execution := cacheExecutions[cacheJobStatusPath(id)]; execution != nil {
		if execution.finished {
			if err := finishCacheExecutionLocked(status, execution); err != nil {
				return status, err
			}
			return readCacheJobStatus(id)
		}
		status.CachedBytes = execution.task.CachedBytes()
		if status.CachedBytes < before.CachedBytes {
			status.CachedBytes = before.CachedBytes
		}
		status.ActiveSeconds = execution.baseSeconds + time.Since(execution.started).Seconds()
	} else if cacheActive(status.State) {
		if status.State == "canceling" {
			status.State = "canceled"
			status.Error = ""
		} else {
			status.State = "interrupted"
			status.Error = "Download interrupted because the task is no longer running; resume manually"
		}
	} else if status.State == "stopped" {
		status.State = "paused"
		status.Error = ""
	}
	if status != before {
		if err = commitCacheSnapshot(before, &status); err != nil {
			return before, err
		}
	}
	return status, nil
}

func finishCacheExecutionLocked(status CacheJobStatus, execution *cacheExecution) error {
	if status.Generation != execution.generation {
		return fmt.Errorf("stale cache execution")
	}
	if execution.trace.Active.Load() != 0 {
		return fmt.Errorf("cache source requests still active")
	}
	execution.trace.Stop()
	before := status
	status.CachedBytes = execution.task.CachedBytes()
	if status.CachedBytes < before.CachedBytes {
		status.CachedBytes = before.CachedBytes
	}
	status.ActiveSeconds = execution.baseSeconds + execution.finishedAt.Sub(execution.started).Seconds()
	status.Error, status.ErrorCode = "", ""
	switch status.State {
	case "pausing":
		status.State = "paused"
	case "canceling":
		status.State = "canceled"
	default:
		status.State = "complete"
		if execution.result != nil {
			status.State = "failed"
			status.Error = execution.result.Error()
			if execution.task.InitialState == "resuming" && !execution.startedWork {
				status.State = "paused"
			}
			if errors.Is(execution.result, context.Canceled) && execution.serviceCtx.Err() != nil {
				status.State = "interrupted"
				status.Error = "Download interrupted by service shutdown; resume manually"
			}
		}
	}
	// A genuine storage failure must remain visible even during an intentional stop.
	if execution.result != nil && !errors.Is(execution.result, context.Canceled) && (before.State == "pausing" || before.State == "canceling") {
		status.State = "failed"
		status.Error = execution.result.Error()
		status.ErrorCode = "download_failed"
	}
	if err := commitCacheSnapshot(before, &status); err != nil {
		return err
	}
	if !execution.task.LocalJob && execution.task.SchedulerDao != nil && execution.task.Job != nil {
		code := int32(consts.RunningStatusJobBreak)
		if status.State == "complete" {
			code = consts.RunningStatusJobComplete
		}
		if status.State == "paused" || status.State == "canceled" {
			code = consts.RunningStatusJobStop
		}
		job := execution.task.Job
		execution.task.SchedulerDao.ExecUpdateCacheJobStatus(execution.task.TaskNo, code, job.InstanceId, job.Namespace, job.Repo, status.Error, execution.task.StockProcess)
	}
	if !execution.stopStarted.IsZero() {
		cacheStopDuration.WithLabelValues(before.State).Observe(execution.finishedAt.Sub(execution.stopStarted).Seconds())
	}
	zap.S().Infow("Finished cache execution", "job", status.ID, "generation", execution.generation, "state", status.State, "sourceRequests", execution.trace.Requests.Load(), "sourceCancellations", execution.trace.Canceled.Load(), "cacheWrittenBytes", execution.trace.WrittenBytes.Load())
	delete(cacheExecutions, cacheJobStatusPath(status.ID))
	return nil
}

func trackCacheJob(preheat *task.PreheatCacheTask, key repository.RepoKey, serviceCtx context.Context) error {
	cacheStateMu.Lock()
	defer cacheStateMu.Unlock()
	previous, err := readCacheJobStatus(int64(preheat.TaskNo))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	path := cacheJobStatusPath(int64(preheat.TaskNo))
	if cacheExecutions[path] != nil {
		return fmt.Errorf("cache execution already active")
	}
	status := CacheJobStatus{ID: int64(preheat.TaskNo), RepoKey: key, Commit: preheat.Sha.Sha, State: "running", TotalBytes: preheat.UsedStorage, Local: preheat.LocalJob,
		Generation: previous.Generation + 1, CachedBytes: previous.CachedBytes, ActiveSeconds: previous.ActiveSeconds}
	if preheat.InitialState != "" {
		status.State = preheat.InitialState
	}
	if preheat.Job != nil {
		status.InstanceID = preheat.Job.InstanceId
	}
	if err := commitCacheSnapshot(previous, &status); err != nil {
		return err
	}
	execution := &cacheExecution{task: preheat, generation: status.Generation, serviceCtx: serviceCtx, started: time.Now(), baseSeconds: previous.ActiveSeconds}
	execution.trace = &common.CacheTransferTrace{}
	execution.state = status.State
	if preheat.Ctx != nil {
		preheat.Ctx = common.WithCacheTrace(preheat.Ctx, execution.trace)
	}
	cacheExecutions[path] = execution
	preheat.OnStart = func() { cacheStateMu.Lock(); defer cacheStateMu.Unlock(); execution.startedWork = true }
	preheat.OnFinish = func(taskErr error) {
		cacheStateMu.Lock()
		defer cacheStateMu.Unlock()
		if cacheExecutions[path] != execution {
			zap.S().Warnw("Ignored stale cache task completion", "job", status.ID, "generation", execution.generation)
			return
		}
		execution.finished, execution.result = true, taskErr
		execution.finishedAt = time.Now()
		current, err := readCacheJobStatus(status.ID)
		if err == nil {
			err = finishCacheExecutionLocked(current, execution)
		}
		if err != nil {
			zap.S().Errorw("Could not persist cache task result", "job", status.ID, "error", err)
		}
	}
	return nil
}
