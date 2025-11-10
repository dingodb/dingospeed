package task

import (
	"context"
	"fmt"
	"sync"

	"dingospeed/internal/dao"
	"dingospeed/internal/downloader"
	"dingospeed/internal/model/query"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type CacheTask struct {
	TaskNo       int
	Ctx          context.Context
	CancelFunc   context.CancelFunc
	Job          *query.CreateCacheJobReq
	SchedulerDao *dao.SchedulerDao
}

func (c *CacheTask) GetTaskNo() int {
	return c.TaskNo
}

func (c *CacheTask) GetCancelFun() context.CancelFunc {
	return c.CancelFunc
}

type PreheatCacheTask struct {
	CacheTask
	Sha           *dao.CommitHfSha
	Authorization string
	FileDao       *dao.FileDao
	DownloaderDao *dao.DownloaderDao
}

func (p *PreheatCacheTask) DoTask() {
	// 获取模型元数据
	orgRepo := fmt.Sprintf("%s/%s", p.Job.Org, p.Job.Repo)
	err := p.preheatProcess(orgRepo)
	if err != nil {
		zap.S().Errorf("preheatProcess,%s", err.Error())
		p.SchedulerDao.ExecUpdateCacheJobStatus(p.TaskNo, consts.StatusCacheJobBreak, p.Job.InstanceId, p.Job.Org, p.Job.Repo, err.Error())
		return
	}
	p.SchedulerDao.ExecUpdateCacheJobStatus(p.TaskNo, consts.StatusCacheJobComplete, p.Job.InstanceId, p.Job.Org, p.Job.Repo, "")
}

func (p *PreheatCacheTask) preheatProcess(orgRepo string) error {
	limit := make(chan struct{}, 8)
	for _, rFile := range p.Sha.Siblings {
		if p.Ctx.Err() != nil {
			return p.Ctx.Err()
		}
		fileName := rFile.Rfilename
		infos, err := p.FileDao.GetPathsInfo(p.Job.Datatype, orgRepo, p.Sha.Sha,
			p.Authorization, []string{fileName})
		if err != nil {
			zap.S().Errorf("RemoteRequestPathsInfo err,%v", err)
			return err
		}
		if len(infos) != 1 {
			zap.S().Errorf("RemoteRequestPathsInfo err, len infos greater than 1")
			return err
		}
		pathInfo := infos[0]
		var etag string
		if pathInfo.Lfs.Oid != "" {
			etag = pathInfo.Lfs.Oid
		} else {
			etag = pathInfo.Oid
		}
		offset := p.FileDao.GetFileOffset(p.Job.Datatype, p.Job.Org, p.Job.Repo, etag, pathInfo.Size)
		if offset < pathInfo.Size {
			limit <- struct{}{}
			if err = p.startPreheat(orgRepo, fileName, p.Sha.Sha, etag, p.Authorization, pathInfo.Size, offset); err != nil {
				zap.S().Errorf("startPreheat err.%v", err)
				return err
			}
			<-limit
		}
	}
	return nil
}

func (p *PreheatCacheTask) startPreheat(orgRepo, fileName, commit, etag, authorization string, fileSize, offset int64) error {
	var wg sync.WaitGroup
	bgCtx := context.WithValue(p.Ctx, consts.PromSource, "localhost")
	responseChan := make(chan []byte, config.SysConfig.Download.RespChanSize)
	ctx, cancel := context.WithCancel(bgCtx)
	defer func() {
		cancel()
	}()
	var hfUri string
	if p.Job.Datatype == "models" {
		hfUri = fmt.Sprintf("/%s/resolve/%s/%s", orgRepo, commit, fileName)
	} else {
		hfUri = fmt.Sprintf("/%s/%s/resolve/%s/%s", p.Job.Datatype, orgRepo, commit, fileName)
	}
	blobsDir := fmt.Sprintf("%s/files/%s/%s/blobs", config.SysConfig.Repos(), p.Job.Datatype, orgRepo)
	blobsFile := fmt.Sprintf("%s/%s", blobsDir, etag)
	filesDir := fmt.Sprintf("%s/files/%s/%s/resolve/%s", config.SysConfig.Repos(), p.Job.Datatype, orgRepo, commit)
	filesPath := fmt.Sprintf("%s/%s", filesDir, fileName)
	if err := p.FileDao.ConstructBlobsAndFileFile(blobsFile, filesPath); err != nil {
		zap.S().Errorf("ConstructBlobsAndFileFile err.%v", err)
		return err
	}
	taskParam := &downloader.TaskParam{
		TaskNo:        0,
		BlobsFile:     blobsFile,
		FileName:      fileName,
		FileSize:      fileSize,
		OrgRepo:       orgRepo,
		Authorization: authorization,
		Uri:           hfUri,
		DataType:      p.Job.Datatype,
		Etag:          etag,
		Preheat:       true,
	}
	taskParam.Context = ctx
	taskParam.ResponseChan = responseChan
	taskParam.Cancel = cancel
	wg.Add(2)
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		return p.result(ctx, responseChan)
	})
	eg.Go(func() error {
		p.DownloaderDao.FileDownload(offset, fileSize, false, taskParam)
		return nil
	})
	return eg.Wait()
}

func (p *PreheatCacheTask) result(ctx context.Context, responseChan chan []byte) error {
	for {
		select {
		case _, ok := <-responseChan:
			if !ok {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
