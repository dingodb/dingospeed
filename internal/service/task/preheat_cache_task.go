package task

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

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
	UsedStorage   uint64
	stockLen      atomic.Uint64
	StockSpeed    string
	StockProcess  float32
}

func (p *PreheatCacheTask) DoTask() {
	orgRepo := fmt.Sprintf("%s/%s", p.Job.Org, p.Job.Repo)
	ctx, cancelFunc := context.WithCancel(p.Ctx)
	defer cancelFunc()
	go p.realTimeSpeed(ctx)
	err := p.preheatProcess(orgRepo)
	if err != nil {
		p.SchedulerDao.ExecUpdateCacheJobStatus(p.TaskNo, consts.StatusCacheJobBreak, p.Job.InstanceId, p.Job.Org, p.Job.Repo, err.Error(), p.StockProcess)
		return
	}
	p.StockProcess = 100
	p.SchedulerDao.ExecUpdateCacheJobStatus(p.TaskNo, consts.StatusCacheJobComplete, p.Job.InstanceId, p.Job.Org, p.Job.Repo, "", p.StockProcess)
}

func (p *PreheatCacheTask) preheatProcess(orgRepo string) error {
	limit := make(chan struct{}, 8)
	for _, rFile := range p.Sha.Siblings {
		if p.Ctx.Err() != nil {
			return p.Ctx.Err()
		}
		fileName := rFile.Rfilename
		var hfUri string
		if p.Job.Datatype == "models" {
			hfUri = fmt.Sprintf("/%s/resolve/%s/%s", orgRepo, p.Sha.Sha, fileName)
		} else {
			hfUri = fmt.Sprintf("/%s/%s/resolve/%s/%s", p.Job.Datatype, orgRepo, p.Sha.Sha, fileName)
		}
		pathInfo, err := p.FileDao.GetPathsInfo(hfUri, p.Job.Datatype, orgRepo, p.Sha.Sha,
			p.Authorization, fileName) // 获取模型元数据
		if err != nil {
			zap.S().Errorf("RemoteRequestPathsInfo err,%v", err)
			return err
		}
		if pathInfo == nil {
			return fmt.Errorf("RemoteRequestPathsInfo err, pathInfo is null, %s/%s", orgRepo, fileName)
		}
		var etag string
		if pathInfo.Lfs.Oid != "" {
			etag = pathInfo.Lfs.Oid
		} else {
			etag = pathInfo.Oid
		}
		if pathInfo.Size == 0 {
			continue
		}
		offset := p.FileDao.GetFileOffset(p.Job.Datatype, p.Job.Org, p.Job.Repo, etag, pathInfo.Size)
		if offset > 0 {
			p.stockLen.Add(uint64(offset))
		}
		if offset < pathInfo.Size {
			limit <- struct{}{}
			if err = p.startPreheat(hfUri, orgRepo, fileName, p.Sha.Sha, etag, p.Authorization, pathInfo.Size, offset); err != nil {
				zap.S().Errorf("startPreheat err, %s/%s %v", orgRepo, fileName, err)
				return err
			}
			<-limit
		}
	}
	return nil
}

func (p *PreheatCacheTask) startPreheat(hfUri, orgRepo, fileName, commit, etag, authorization string, fileSize, offset int64) error {
	var wg sync.WaitGroup
	bgCtx := context.WithValue(p.Ctx, consts.PromSource, "localhost")
	responseChan := make(chan []byte, config.SysConfig.Download.RespChanSize)
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
	}
	taskParam.Context = bgCtx
	taskParam.ResponseChan = responseChan
	taskParam.Cancel = p.CancelFunc
	wg.Add(2)
	eg, ctx := errgroup.WithContext(bgCtx)
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
		case b, ok := <-responseChan:
			if !ok {
				return nil
			}
			p.stockLen.Add(uint64(len(b)))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *PreheatCacheTask) realTimeSpeed(ctx context.Context) {
	lastBytes := uint64(0)
	ticker := time.NewTicker(1 * time.Second) // 1 秒采样一次
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			currentBytes := p.stockLen.Load()
			delta := currentBytes - lastBytes
			p.StockSpeed = formatSpeed(delta, 1*time.Second) // 采样间隔 1 秒
			// 计算下载进度（百分比）
			process := float64(currentBytes) / float64(p.UsedStorage) * 100
			if process >= 100 {
				process = 99.9
			}
			p.StockProcess = float32(math.Round(process*10) / 10)
			lastBytes = currentBytes
		case <-ctx.Done():
			zap.S().Debug("speed ctx done")
			p.StockSpeed = "0 B/s"
			return
		}
	}
}

func formatSpeed(bytes uint64, duration time.Duration) string {
	if duration <= 0 || bytes <= 0 {
		return "0 B/s"
	}
	speedBps := float64(bytes) / duration.Seconds()
	switch {
	case speedBps >= 1024*1024:
		return fmt.Sprintf("%.2f MB/s", speedBps/(1024*1024))
	case speedBps >= 1024:
		return fmt.Sprintf("%.2f KB/s", speedBps/1024)
	default:
		return fmt.Sprintf("%.2f B/s", speedBps)
	}
}
