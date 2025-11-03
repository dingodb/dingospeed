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
	"dingospeed/pkg/proto/manager"

	"go.uber.org/zap"
)

type CacheTask struct {
	TaskNo       int
	Ctx          context.Context
	CancelFunc   context.CancelFunc
	Job          *query.CacheJobQuery
	SchedulerDao *dao.SchedulerDao
	InstanceId   string
}

func (c *CacheTask) GetTaskNo() int {
	return c.TaskNo
}

func (c *CacheTask) GetCancelFun() context.CancelFunc {
	return c.CancelFunc
}

func (c *CacheTask) UpdateCacheJobStatus(status int32, errorMsg string) {
	zap.S().Infof("update status jobId:%d,status:%d, %s", c.Job.Id, status, errorMsg)
	c.SchedulerDao.UpdateCacheJobStatus(&manager.UpdateCacheJobStatusReq{
		Id:         int64(c.Job.Id),
		InstanceId: c.InstanceId,
		Status:     status,
		ErrorMsg:   errorMsg,
	})
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
	zap.S().Infof("preheat start...%s", orgRepo)
	err := p.preheatProcess(orgRepo)
	if err != nil {
		zap.S().Errorf("999999999999999999999,%s", err.Error())
		p.UpdateCacheJobStatus(consts.StatusCacheJobBreak, err.Error())
		return
	}
	zap.S().Infof("preheat end.%s", orgRepo)
	p.UpdateCacheJobStatus(consts.StatusCacheJobComplete, "")
}

func (p *PreheatCacheTask) preheatProcess(orgRepo string) error {
	limit := make(chan struct{}, 8)
	for _, rFile := range p.Sha.Siblings {
		if p.Ctx.Err() != nil {
			zap.S().Errorf("777777777777777777777,%s", p.Ctx.Err().Error())
			p.UpdateCacheJobStatus(consts.StatusCacheJobBreak, p.Ctx.Err().Error())
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
	go func() {
		defer func() {
			wg.Done()
		}()
		p.result(ctx, responseChan)
	}()
	go func() {
		defer func() {
			wg.Done()
		}()
		p.DownloaderDao.FileDownload(offset, fileSize, false, taskParam)
	}()
	wg.Wait()
	return nil
}

func (p *PreheatCacheTask) result(ctx context.Context, responseChan chan []byte) {
	for {
		select {
		case _, ok := <-responseChan:
			if !ok {
				return
			}
		case <-ctx.Done():
			zap.S().Errorf("000000000000000000000000000")
			// zap.S().Errorf("000000000000000000000000000,%s", Ctx.Err().Error())
			p.UpdateCacheJobStatus(consts.StatusCacheJobBreak, ctx.Err().Error())
			return
		}
	}
}
