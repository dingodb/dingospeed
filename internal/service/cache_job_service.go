package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/model/query"
	task2 "dingospeed/internal/service/task"
	"dingospeed/pkg/app"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/proto/manager"
	"dingospeed/pkg/repository"

	"github.com/labstack/echo/v4"
)

type CacheJobService struct {
	createMu      sync.Mutex
	fileDao       *dao.FileDao
	metaDao       *dao.MetaDao
	downloaderDao *dao.DownloaderDao
	schedulerDao  *dao.SchedulerDao
	cachePool     *common.Pool
}

func NewCacheJobService(fileDao *dao.FileDao, metaDao *dao.MetaDao, downloaderDao *dao.DownloaderDao, schedulerDao *dao.SchedulerDao) *CacheJobService {
	p := &CacheJobService{
		fileDao:       fileDao,
		metaDao:       metaDao,
		downloaderDao: downloaderDao,
		schedulerDao:  schedulerDao,
		cachePool:     common.NewPool(30, true),
	}
	registrationCachePool = p.cachePool
	return p
}

func (p *CacheJobService) CreateCacheJob(c echo.Context, jobReq *query.CreateCacheJobReq) (int64, error) {
	result, err := p.CreateCacheJobResult(c, jobReq)
	return result.ID, err
}

func (p *CacheJobService) CreateCacheJobResult(c echo.Context, jobReq *query.CreateCacheJobReq) (CacheJobCreateResult, error) {
	registrationAdmission.Lock()
	defer registrationAdmission.Unlock()
	localJob, instanceID, err := p.cacheJobIdentity(jobReq.InstanceId)
	if err != nil {
		return CacheJobCreateResult{}, err
	}
	jobReq.InstanceId = instanceID
	if jobReq.Namespace == "" {
		k, err := legacyJobKey(jobReq.Datatype, jobReq.Org, jobReq.Repo, jobReq.OrgRepo)
		if err != nil {
			return CacheJobCreateResult{}, err
		}
		jobReq.Namespace, jobReq.Repo = k.Namespace, k.Repo
	} else if jobReq.Org != "" || jobReq.OrgRepo != "" {
		return CacheJobCreateResult{}, fmt.Errorf("conflicting job identity")
	}

	key := repository.RepoKey{Namespace: jobReq.Namespace, RepoType: jobReq.Datatype, Repo: jobReq.Repo}
	if err := key.Validate(); err != nil {
		return CacheJobCreateResult{}, err
	}
	if key.Namespace == repository.HuggingFace || key.Namespace == repository.ModelScope {
		if err := repository.Register(config.SysConfig.Repos(), repository.Remote(key)); err != nil {
			return CacheJobCreateResult{}, err
		}
	} else if _, err := repository.Read(config.SysConfig.Repos(), key); err != nil {
		return CacheJobCreateResult{}, err
	}

	appInfo, _ := app.FromContext(c.Request().Context())
	ctx, cancelFunc := context.WithCancel(appInfo.Ctx())
	var task common.Task
	cacheTask := task2.CacheTask{
		Ctx:           ctx,
		Job:           jobReq,
		CancelFunc:    cancelFunc,
		SchedulerDao:  p.schedulerDao,
		RunningStatus: consts.RunningStatusJobBreak,
		LocalJob:      localJob,
	}
	req := &manager.CreateCacheJobReq{
		Type:       jobReq.Type,
		InstanceId: jobReq.InstanceId,
		Datatype:   jobReq.Datatype,
		Org:        jobReq.Namespace,
		Repo:       jobReq.Repo,
		Status:     consts.RunningStatusJobIng,
	}
	authorization := c.Request().Header.Get("Authorization")
	if jobReq.Type == consts.CacheTypePreheat {
		revision := "main"
		if key.Namespace == repository.ModelScope {
			revision = "master"
		}
		sha, err := p.preheatMetadata(ctx, key, revision, authorization)
		if err != nil {
			cancelFunc()
			return CacheJobCreateResult{}, err
		}
		// Metadata resolves the requested branch before comparing immutable commits.
		// Hold admission until the new job is registered AND submitted to the pool.
		p.createMu.Lock()
		defer p.createMu.Unlock()
		existing, err := p.reusableCacheJob(key, sha, localJob, instanceID)
		if err != nil {
			cancelFunc()
			return CacheJobCreateResult{}, err
		}
		if existing != nil {
			cancelFunc()
			disposition := "running"
			if existing.State == "complete" {
				disposition = "cached"
			}
			return CacheJobCreateResult{ID: existing.ID, Msg: "success", Disposition: disposition, Commit: sha.Sha}, nil
		}
		req.UsedStorage = sha.UsedStorage
		req.Commit = sha.Sha
		var jobID int64
		if localJob {
			jobID, err = reserveLocalCacheJob()
		} else {
			var cacheJob *manager.CreateCacheJobResp
			cacheJob, err = p.schedulerDao.CreateCacheJob(req)
			if err == nil {
				jobID = cacheJob.Id
			}
		}
		if err != nil {
			cancelFunc()
			return CacheJobCreateResult{}, err
		}
		cacheTask.TaskNo = int(jobID)
		task = &task2.PreheatCacheTask{
			CacheTask:     cacheTask,
			FileDao:       p.fileDao,
			DownloaderDao: p.downloaderDao,
			Sha:           sha,
			Authorization: authorization,
			UsedStorage:   uint64(sha.UsedStorage),
		}
		if err := trackCacheJob(task.(*task2.PreheatCacheTask), key, appInfo.Ctx()); err != nil {
			cancelFunc()
			return CacheJobCreateResult{}, err
		}
		if err = p.cachePool.SubmitForTimeout(ctx, task); err != nil {
			task.(*task2.PreheatCacheTask).OnFinish(err)
			if !localJob {
				p.schedulerDao.ExecUpdateCacheJobStatus(int(jobID), consts.RunningStatusJobWait, jobReq.InstanceId, "", "", consts.TaskMoreErrMsg, 0)
			}
			cancelFunc()
			return CacheJobCreateResult{}, err
		}
	} else if jobReq.Type == consts.CacheTypeMount {
		cacheTask.TaskNo = int(jobReq.RepositoryId)
		task = &task2.MountCacheTask{
			CacheTask:     cacheTask,
			Authorization: authorization,
		}
		if err := p.cachePool.SubmitForTimeout(ctx, task); err != nil {
			p.schedulerDao.ExecUpdateRepositoryMountStatus(cacheTask.TaskNo, consts.RunningStatusJobWait, consts.TaskMoreErrMsg)
		}
	} else {
		defer cancelFunc()
		return CacheJobCreateResult{}, fmt.Errorf("cache job type is err,%d", jobReq.Type)
	}
	return CacheJobCreateResult{ID: int64(cacheTask.TaskNo), Msg: "success", Disposition: "created", Commit: req.Commit}, nil
}

type CacheJobConflict struct {
	Code        string
	ActiveJobID int64
}

func (e *CacheJobConflict) Error() string { return e.Code }

func (p *CacheJobService) StopCacheJob(req *query.JobStatusReq) error {
	if req.Intent == "" {
		req.Intent = "pause"
	}
	if req.Intent != "pause" && req.Intent != "cancel" {
		return fmt.Errorf("invalid stop intent")
	}
	cacheStateMu.Lock()
	defer cacheStateMu.Unlock()
	status, err := cacheJobSnapshotLocked(req.Id)
	if errors.Is(err, os.ErrNotExist) {
		if existing, ok := p.cachePool.GetTask(int(req.Id)); ok {
			if mount, ok := existing.(*task2.MountCacheTask); ok {
				mount.RequestStop()
				return nil
			}
		}
	}
	if err != nil {
		return err
	}
	before := status
	if status.State == "complete" || status.State == "canceled" {
		return nil
	}
	execution := cacheExecutions[cacheJobStatusPath(req.Id)]
	if execution == nil {
		if req.Intent == "cancel" {
			status.State = "canceled"
		} else if status.State == "paused" || status.State == "interrupted" {
			status.State = "paused"
		} else {
			return &CacheJobConflict{Code: "invalid_task_state"}
		}
	} else {
		if req.Intent == "cancel" || status.State == "canceling" {
			status.State = "canceling"
		} else {
			status.State = "pausing"
		}
	}
	status.Error, status.ErrorCode = "", ""
	if status != before {
		if err := commitCacheSnapshot(before, &status); err != nil {
			return err
		}
	}
	if execution != nil {
		if execution.stopStarted.IsZero() {
			execution.stopStarted = time.Now()
		}
		execution.task.CancelFunc()
	}
	return nil
}

func (p *CacheJobService) ResumeCacheJob(c echo.Context, req *query.ResumeCacheJobReq) error {
	registrationAdmission.Lock()
	defer registrationAdmission.Unlock()
	// Create and resume share admission; no second job may write the same commit.
	p.createMu.Lock()
	defer p.createMu.Unlock()
	previous, err := p.CacheJobStatus(req.Id)
	if err != nil {
		return err
	}
	if cacheActive(previous.State) {
		if previous.State == "pausing" || previous.State == "canceling" {
			return &CacheJobConflict{Code: "task_stopping"}
		}
		return nil
	}
	if previous.State != "paused" && previous.State != "interrupted" {
		return &CacheJobConflict{Code: "invalid_task_state"}
	}
	if previous.Commit == "" {
		return &CacheJobConflict{Code: "missing_pinned_commit"}
	}
	local, instance, err := p.resumeCacheJobIdentity(previous, req.InstanceId)
	if err != nil {
		return err
	}
	if _, active := p.cachePool.GetTask(int(req.Id)); active {
		return &CacheJobConflict{Code: "task_stopping"}
	}
	existing, err := p.reusableCacheJob(previous.RepoKey, &dao.CommitHfSha{Sha: previous.Commit}, local, instance)
	if err != nil {
		return err
	}
	if existing != nil && existing.ID != req.Id && cacheActive(existing.State) {
		return &CacheJobConflict{Code: "active_job_conflict", ActiveJobID: existing.ID}
	}
	appInfo, _ := app.FromContext(c.Request().Context())
	ctx, cancel := context.WithCancel(appInfo.Ctx())
	preheat := &task2.PreheatCacheTask{
		CacheTask: task2.CacheTask{TaskNo: int(req.Id), Ctx: ctx, CancelFunc: cancel, LocalJob: local, SchedulerDao: p.schedulerDao,
			Job: &query.CreateCacheJobReq{InstanceId: instance, Type: consts.CacheTypePreheat, Namespace: previous.Namespace, Repo: previous.Repo, Datatype: previous.RepoType}},
		Sha: &dao.CommitHfSha{Sha: previous.Commit}, UsedStorage: previous.TotalBytes, FileDao: p.fileDao, DownloaderDao: p.downloaderDao,
		Authorization: c.Request().Header.Get("Authorization"), InitialState: "resuming",
	}
	if err := trackCacheJob(preheat, previous.RepoKey, appInfo.Ctx()); err != nil {
		cancel()
		return err
	}
	go func() {
		sha, err := p.preheatMetadata(ctx, previous.RepoKey, previous.Commit, preheat.Authorization)
		if err == nil && sha.Sha != previous.Commit {
			err = fmt.Errorf("upstream changed pinned commit")
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			preheat.OnFinish(err)
			cancel()
			return
		}
		preheat.Sha = sha
		preheat.UsedStorage = uint64(sha.UsedStorage)
		cacheStateMu.Lock()
		current, err := readCacheJobStatus(req.Id)
		if err == nil && current.State == "resuming" {
			next := current
			next.State = "running"
			next.TotalBytes = preheat.UsedStorage
			err = commitCacheSnapshot(current, &next)
		} else if err == nil {
			err = context.Canceled
		}
		cacheStateMu.Unlock()
		if err == nil {
			err = p.cachePool.SubmitForTimeout(ctx, preheat)
		}
		if err != nil {
			preheat.OnFinish(err)
			cancel()
		}
	}()
	return nil
}

func (p *CacheJobService) RealtimeCacheJob(realtimeReq *query.RealtimeReq) []*query.RealtimeResp {
	ret := make([]*query.RealtimeResp, 0)
	for _, jobId := range realtimeReq.CacheJobIds {
		if task, ok := p.cachePool.GetTask(int(jobId)); ok {
			if pTask, ok := task.(*task2.PreheatCacheTask); ok {
				speed, progress := pTask.RealtimeProgress()
				ret = append(ret, &query.RealtimeResp{
					CacheJobId:   jobId,
					StockSpeed:   speed,
					StockProcess: progress,
				})
			}
		}
	}
	return ret
}

func (p *CacheJobService) preheatMetadata(ctx context.Context, key repository.RepoKey, revision, authorization string) (*dao.CommitHfSha, error) {
	if key.Namespace == repository.HuggingFace {
		meta, err := p.metaDao.RefreshPreheatMetadataContext(ctx, key, revision, authorization)
		if err != nil {
			return nil, err
		}
		// HF usedStorage may include multiple versions; count the pinned snapshot.
		meta.UsedStorage = 0
		prefix := ""
		if key.RepoType != "models" {
			prefix = "/" + key.RepoType
		}
		for _, file := range meta.Siblings {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := repository.Relative(file.Rfilename); err != nil {
				return nil, err
			}
			uri := prefix + "/" + repository.EscapeURLPath(key.Repo) + "/resolve/" + repository.EscapeURLPath(meta.Sha) + "/" + repository.EscapeURLPath(file.Rfilename)
			info, err := p.fileDao.GetPathsInfoContext(ctx, uri, key.RepoType, key.ID(), meta.Sha, authorization, file.Rfilename)
			if err != nil {
				return nil, err
			}
			if info == nil || info.Size < 0 || info.Size > math.MaxInt64-meta.UsedStorage {
				return nil, fmt.Errorf("invalid snapshot file size")
			}
			meta.UsedStorage += info.Size
		}
		return meta, nil
	}
	return task2.ReadRepositoryMetadata(ctx, key, revision, authorization)
}

func legacyJobKey(typ, org, repo, id string) (repository.RepoKey, error) {
	if id != "" {
		if org != "" || repo != "" {
			if strings.TrimPrefix(org+"/"+repo, "/") != id {
				return repository.RepoKey{}, fmt.Errorf("conflicting orgRepo")
			}
		} else {
			parts := strings.SplitN(id, "/", 2)
			if len(parts) == 1 {
				repo = id
			} else {
				org, repo = parts[0], parts[1]
			}
		}
	}
	return repository.FromStorage(typ, org, repo)
}

// Task authority follows durable registration even while a cluster is degraded.
func (p *CacheJobService) resumeCacheJobIdentity(previous CacheJobStatus, requested string) (bool, string, error) {
	if previous.Local {
		// The durable record belongs to this data root. Joining a scheduler
		// does not transfer ownership of an existing standalone job.
		if requested != "" && requested != config.SysConfig.Registration().NodeID {
			return false, "", &CacheJobConflict{Code: "task_node_mismatch"}
		}
		return true, "", nil
	}
	local, instance, err := p.cacheJobIdentity(requested)
	if err != nil {
		return false, "", err
	}
	if local || previous.InstanceID != instance {
		return false, "", &CacheJobConflict{Code: "task_node_mismatch"}
	}
	return false, instance, nil
}

func (p *CacheJobService) cacheJobIdentity(requested string) (bool, string, error) {
	registration := config.SysConfig.Registration()
	if state := config.SysConfig.RegistrationState(); state.State == "error" {
		return false, "", fmt.Errorf("invalid scheduler registration: %s", state.Error)
	}
	if !registration.Enabled {
		return true, "", nil
	}
	if requested != "" && requested != registration.NodeID {
		return false, "", fmt.Errorf("cache job node identity mismatch")
	}
	if registration.NodeID == "" || p.schedulerDao == nil || p.schedulerDao.Client == nil {
		return false, "", fmt.Errorf("scheduler unavailable")
	}
	return false, registration.NodeID, nil
}
