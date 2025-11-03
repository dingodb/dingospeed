package service

import (
	"context"
	"fmt"

	"dingospeed/internal/dao"
	"dingospeed/internal/model/query"
	task2 "dingospeed/internal/service/task"
	"dingospeed/pkg/app"
	"dingospeed/pkg/common"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/proto/manager"

	"github.com/bytedance/sonic"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type CacheJobService struct {
	fileDao       *dao.FileDao
	metaDao       *dao.MetaDao
	downloaderDao *dao.DownloaderDao
	schedulerDao  *dao.SchedulerDao
	cachePool     *common.Pool
}

func NewCacheJobService(fileDao *dao.FileDao, metaDao *dao.MetaDao, downloaderDao *dao.DownloaderDao, schedulerDao *dao.SchedulerDao) *CacheJobService {
	return &CacheJobService{
		fileDao:       fileDao,
		metaDao:       metaDao,
		downloaderDao: downloaderDao,
		schedulerDao:  schedulerDao,
		cachePool:     common.NewPool(30, true),
	}
}

func (p *CacheJobService) CreateCacheJob(c echo.Context, job *query.CacheJobQuery) (int64, error) {
	appInfo, _ := app.FromContext(c.Request().Context())
	ctx, cancelFunc := context.WithCancel(appInfo.Ctx())
	var task common.Task
	cacheTask := task2.CacheTask{
		Ctx:          ctx,
		Job:          job,
		CancelFunc:   cancelFunc,
		SchedulerDao: p.schedulerDao,
		InstanceId:   job.InstanceId,
	}
	req := &manager.CreateCacheJobReq{
		Type:       job.Type,
		InstanceId: job.InstanceId,
		Datatype:   job.Datatype,
		Org:        job.Org,
		Repo:       job.Repo,
		Token:      job.Token,
		Status:     consts.StatusCacheJobIng,
	}
	if job.Type == consts.CacheTypePreheat {
		orgRepo := fmt.Sprintf("%s/%s", job.Org, job.Repo)
		authorization := fmt.Sprintf("Bearer %s", job.Token)
		metadata, err := p.metaDao.GetMetadata(job.Datatype, orgRepo, "main", "get", authorization)
		if err != nil {
			return 0, err
		}
		var sha dao.CommitHfSha
		if err = sonic.Unmarshal(metadata.OriginContent, &sha); err != nil {
			zap.S().Errorf("unmarshal content error:%v", err)
			return 0, err
		}
		req.UsedStorage = sha.UsedStorage
		req.Commit = sha.Sha
		cacheJob, err := p.schedulerDao.CreateCacheJob(req)
		if err != nil {
			return 0, err
		}
		cacheTask.TaskNo = int(cacheJob.Id)
		task = &task2.PreheatCacheTask{
			CacheTask:     cacheTask,
			FileDao:       p.fileDao,
			DownloaderDao: p.downloaderDao,
			Sha:           &sha,
			Authorization: authorization,
		}
		if err := p.cachePool.Submit(ctx, task); err != nil {
			return 0, err
		}
	} else if job.Type == consts.CacheTypeMount {
		cacheJob, err := p.schedulerDao.CreateCacheJob(req)
		if err != nil {
			return 0, err
		}
		cacheTask.TaskNo = int(cacheJob.Id)
		task = &task2.MountCacheTask{
			CacheTask: cacheTask,
		}
		if err := p.cachePool.Submit(ctx, task); err != nil {
			return 0, err
		}
	} else {
		defer cancelFunc()
		return 0, fmt.Errorf("cache job type is err,%d", job.Type)
	}
	return int64(cacheTask.TaskNo), nil
}

func (p *CacheJobService) StopCacheJob(jobStatus *query.JobStatus) error {
	if task, ok := p.cachePool.GetTask(int(jobStatus.Id)); ok {
		cancelFun := task.GetCancelFun()
		cancelFun()
	} else {
		return fmt.Errorf("无法执行停止操作，没有相关任务。jobId:%d", jobStatus.Id)
	}
	return nil
}

func (p *CacheJobService) ResumeCacheJob(c echo.Context, job *query.CacheJobQuery) error {
	appInfo, _ := app.FromContext(c.Request().Context())
	ctx, cancelFunc := context.WithCancel(appInfo.Ctx())
	var task common.Task
	cacheTask := task2.CacheTask{
		TaskNo:       int(job.Id),
		Ctx:          ctx,
		Job:          job,
		CancelFunc:   cancelFunc,
		SchedulerDao: p.schedulerDao,
		InstanceId:   job.InstanceId,
	}
	if job.Type == consts.CacheTypePreheat {
		orgRepo := fmt.Sprintf("%s/%s", job.Org, job.Repo)
		authorization := fmt.Sprintf("Bearer %s", job.Token)
		metadata, err := p.metaDao.GetMetadata(job.Datatype, orgRepo, "main", "get", authorization)
		if err != nil {
			return err
		}
		var sha dao.CommitHfSha
		if err = sonic.Unmarshal(metadata.OriginContent, &sha); err != nil {
			zap.S().Errorf("unmarshal content error:%v", err)
			return err
		}
		task = &task2.PreheatCacheTask{
			CacheTask:     cacheTask,
			FileDao:       p.fileDao,
			DownloaderDao: p.downloaderDao,
			Sha:           &sha,
			Authorization: authorization,
		}
		if err := p.cachePool.Submit(ctx, task); err != nil {
			return err
		}
	}
	return nil
}
