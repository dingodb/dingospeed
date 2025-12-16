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

func (p *CacheJobService) CreateCacheJob(c echo.Context, jobReq *query.CreateCacheJobReq) (int64, error) {
	appInfo, _ := app.FromContext(c.Request().Context())
	ctx, cancelFunc := context.WithCancel(appInfo.Ctx())
	var task common.Task
	cacheTask := task2.CacheTask{
		Ctx:           ctx,
		Job:           jobReq,
		CancelFunc:    cancelFunc,
		SchedulerDao:  p.schedulerDao,
		RunningStatus: consts.RunningStatusJobBreak,
	}
	req := &manager.CreateCacheJobReq{
		Type:       jobReq.Type,
		InstanceId: jobReq.InstanceId,
		Datatype:   jobReq.Datatype,
		Org:        jobReq.Org,
		Repo:       jobReq.Repo,
		Status:     consts.RunningStatusJobIng,
	}
	authorization := c.Request().Header.Get("Authorization")
	if jobReq.Type == consts.CacheTypePreheat {
		orgRepo := fmt.Sprintf("%s/%s", jobReq.Org, jobReq.Repo)
		metadata, err := p.metaDao.GetMetadata(jobReq.Datatype, orgRepo, "main", "get", authorization)
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
			UsedStorage:   uint64(sha.UsedStorage),
		}
		if err = p.cachePool.SubmitForTimeout(ctx, task); err != nil {
			p.schedulerDao.ExecUpdateCacheJobStatus(int(cacheJob.Id), consts.RunningStatusJobWait, jobReq.InstanceId, "", "", consts.TaskMoreErrMsg, 0)
			return 0, err
		}
	} else if jobReq.Type == consts.CacheTypeMount {
		cacheTask.TaskNo = int(jobReq.RepositoryId)
		task = &task2.MountCacheTask{
			CacheTask:     cacheTask,
			Authorization: authorization,
		}
		if err := p.cachePool.SubmitForTimeout(ctx, task); err != nil {
			p.schedulerDao.ExecUpdateRepositoryMountStatus(cacheTask.TaskNo, consts.RunningStatusJobWait, consts.TaskMoreErrMsg)
			return 0, err
		}
	} else {
		defer cancelFunc()
		return 0, fmt.Errorf("cache job type is err,%d", jobReq.Type)
	}
	return int64(cacheTask.TaskNo), nil
}

func (p *CacheJobService) StopCacheJob(jobStatusReq *query.JobStatusReq) error {
	if task, ok := p.cachePool.GetTask(int(jobStatusReq.Id)); ok {
		if t, ok := task.(*task2.PreheatCacheTask); ok {
			t.RunningStatus = consts.RunningStatusJobStop
		} else if t, ok := task.(*task2.MountCacheTask); ok {
			t.RunningStatus = consts.RunningStatusJobStop
		}
		cancelFun := task.GetCancelFun()
		cancelFun()
	} else {
		p.schedulerDao.ExecUpdateCacheJobStatus(int(jobStatusReq.Id), consts.RunningStatusJobStop, jobStatusReq.InstanceId, "", "", "speed未注册该任务，下载已中断。", 0)
	}
	return nil
}

func (p *CacheJobService) ResumeCacheJob(c echo.Context, resumeCacheJobReq *query.ResumeCacheJobReq) error {
	appInfo, _ := app.FromContext(c.Request().Context())
	ctx, cancelFunc := context.WithCancel(appInfo.Ctx())
	var task common.Task
	cacheTask := task2.CacheTask{
		TaskNo: int(resumeCacheJobReq.Id),
		Ctx:    ctx,
		Job: &query.CreateCacheJobReq{
			InstanceId: resumeCacheJobReq.InstanceId, Type: resumeCacheJobReq.Type,
			Org: resumeCacheJobReq.Org, Repo: resumeCacheJobReq.Repo,
			Datatype: resumeCacheJobReq.Datatype,
		},
		CancelFunc:   cancelFunc,
		SchedulerDao: p.schedulerDao,
	}
	if resumeCacheJobReq.Type == consts.CacheTypePreheat {
		orgRepo := fmt.Sprintf("%s/%s", resumeCacheJobReq.Org, resumeCacheJobReq.Repo)
		authorization := c.Request().Header.Get("Authorization")
		metadata, err := p.metaDao.GetMetadata(resumeCacheJobReq.Datatype, orgRepo, "main", "get", authorization)
		if err != nil {
			return err
		}
		var sha dao.CommitHfSha
		if err = sonic.Unmarshal(metadata.OriginContent, &sha); err != nil {
			zap.S().Errorf("unmarshal content error:%v", err)
			return err
		}
		// 将状态重置为进行中
		p.schedulerDao.ExecUpdateCacheJobStatus(int(resumeCacheJobReq.Id), consts.RunningStatusJobIng,
			resumeCacheJobReq.InstanceId, resumeCacheJobReq.Org, resumeCacheJobReq.Repo, "", 0)
		task = &task2.PreheatCacheTask{
			CacheTask:     cacheTask,
			FileDao:       p.fileDao,
			DownloaderDao: p.downloaderDao,
			Sha:           &sha,
			Authorization: authorization,
			UsedStorage:   uint64(resumeCacheJobReq.UsedStorage),
		}
		if err := p.cachePool.SubmitForTimeout(ctx, task); err != nil {
			p.schedulerDao.ExecUpdateCacheJobStatus(int(resumeCacheJobReq.Id), consts.RunningStatusJobWait, resumeCacheJobReq.InstanceId, "", "", consts.TaskMoreErrMsg, 0)
			return err
		}
	}
	return nil
}

func (p *CacheJobService) RealtimeCacheJob(realtimeReq *query.RealtimeReq) []*query.RealtimeResp {
	ret := make([]*query.RealtimeResp, 0)
	for _, jobId := range realtimeReq.CacheJobIds {
		if task, ok := p.cachePool.GetTask(int(jobId)); ok {
			if pTask, ok := task.(*task2.PreheatCacheTask); ok {
				ret = append(ret, &query.RealtimeResp{
					CacheJobId:   jobId,
					StockSpeed:   pTask.StockSpeed,
					StockProcess: pTask.StockProcess,
				})
			}
		}
	}
	return ret
}
