package task

import (
	"context"
	"errors"
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
	"dingospeed/pkg/hfprojection"
	"dingospeed/pkg/repository"

	"go.uber.org/zap"
)

type CacheTask struct {
	TaskNo        int
	Ctx           context.Context
	CancelFunc    context.CancelFunc
	Job           *query.CreateCacheJobReq
	SchedulerDao  *dao.SchedulerDao
	RunningStatus int32
	LocalJob      bool
	stopRequested int32
}

func (c *CacheTask) RequestStop() { atomic.StoreInt32(&c.stopRequested, 1); c.CancelFunc() }
func (c *CacheTask) StopStatus() int32 {
	if atomic.LoadInt32(&c.stopRequested) != 0 {
		return consts.RunningStatusJobStop
	}
	return c.RunningStatus
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
	progressMu    sync.RWMutex
	StockSpeed    string
	StockProcess  float32
	OnFinish      func(error)
	OnStart       func()
	InitialState  string
}

func (p *PreheatCacheTask) RealtimeProgress() (string, float32) {
	p.progressMu.RLock()
	defer p.progressMu.RUnlock()
	return p.StockSpeed, p.StockProcess
}

func (p *PreheatCacheTask) CachedBytes() uint64 { return p.stockLen.Load() }

func (p *PreheatCacheTask) DoTask() {
	if p.OnStart != nil {
		p.OnStart()
	}
	orgRepo := fmt.Sprintf("%s/%s", p.Job.Namespace, p.Job.Repo)
	ctx, cancelFunc := context.WithCancel(p.Ctx)
	defer cancelFunc()
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		p.realTimeSpeed(ctx)
	}()
	err := p.preheatProcess(orgRepo)
	// Stop sampling before recording the terminal progress; a late tick must
	// not overwrite 100% with the last in-flight percentage.
	cancelFunc()
	<-progressDone
	if p.OnFinish != nil {
		if err == nil && p.Job.Namespace == repository.HuggingFace {
			hfprojection.DefaultIndex.Invalidate(config.SysConfig.Repos(), p.Job.Datatype)
		}
		p.OnFinish(err)
		return
	}
	if p.LocalJob {
		if err == nil && p.Job.Namespace == repository.HuggingFace {
			hfprojection.DefaultIndex.Invalidate(config.SysConfig.Repos(), p.Job.Datatype)
		}
		return
	}
	if err != nil {
		p.SchedulerDao.ExecUpdateCacheJobStatus(p.TaskNo, p.RunningStatus, p.Job.InstanceId, p.Job.Namespace, p.Job.Repo, err.Error(), p.StockProcess)
		return
	}
	p.StockProcess = 100
	if p.Job.Namespace == repository.HuggingFace {
		hfprojection.DefaultIndex.Invalidate(config.SysConfig.Repos(), p.Job.Datatype)
	}
	p.SchedulerDao.ExecUpdateCacheJobStatus(p.TaskNo, consts.RunningStatusJobComplete, p.Job.InstanceId, p.Job.Namespace, p.Job.Repo, "", p.StockProcess)
}

func (p *PreheatCacheTask) preheatProcess(orgRepo string) error {
	key, err := repository.ParseID(p.Job.Datatype, orgRepo)
	if err != nil {
		return err
	}
	if key.Namespace != repository.HuggingFace {
		return p.preheatViaRepositoryAPI(key)
	}
	limit := make(chan struct{}, 8)
	for _, rFile := range p.Sha.Siblings {
		if p.Ctx.Err() != nil {
			return p.Ctx.Err()
		}
		fileName := rFile.Rfilename
		var hfUri string
		upstream, upErr := dao.UpstreamRepo(p.Job.Datatype, orgRepo)
		if upErr != nil {
			return upErr
		}
		if p.Job.Datatype == "models" {
			hfUri = fmt.Sprintf("/%s/resolve/%s/%s", repository.EscapeURLPath(upstream), repository.EscapeURLPath(p.Sha.Sha), repository.EscapeURLPath(fileName))
		} else {
			hfUri = fmt.Sprintf("/%s/%s/resolve/%s/%s", p.Job.Datatype, repository.EscapeURLPath(upstream), repository.EscapeURLPath(p.Sha.Sha), repository.EscapeURLPath(fileName))
		}
		pathInfo, err := p.FileDao.GetPathsInfoContext(p.Ctx, hfUri, p.Job.Datatype, orgRepo, p.Sha.Sha,
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
		if err := repository.Segment(etag); err != nil {
			return err
		}
		// A reused blob still needs a file reference under this new commit.
		if err := p.FileDao.ConstructBlobsAndFileFile(dao.BlobPath(p.Job.Datatype, orgRepo, etag), dao.ResolvePath(p.Job.Datatype, orgRepo, p.Sha.Sha, fileName)); err != nil {
			return err
		}
		if pathInfo.Size == 0 {
			continue
		}
		offset := p.FileDao.GetFileOffset(p.Job.Datatype, p.Job.Namespace, p.Job.Repo, etag, pathInfo.Size)
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
	bgCtx := context.WithValue(p.Ctx, consts.PromSource, "localhost")
	responseChan := make(chan []byte, config.SysConfig.Download.RespChanSize)
	blobsFile := dao.BlobPath(p.Job.Datatype, orgRepo, etag)
	filesPath := dao.ResolvePath(p.Job.Datatype, orgRepo, commit, fileName)
	if err := p.FileDao.ConstructBlobsAndFileFile(blobsFile, filesPath); err != nil {
		zap.S().Errorf("ConstructBlobsAndFileFile err.%v", err)
		return err
	}
	taskParam := &downloader.TaskParam{
		LocalOnly:     p.LocalJob,
		TaskNo:        0,
		RepoKey:       repository.RepoKey{Namespace: p.Job.Namespace, RepoType: p.Job.Datatype, Repo: p.Job.Repo},
		Revision:      commit,
		BlobsFile:     blobsFile,
		FileName:      fileName,
		FileSize:      fileSize,
		OrgRepo:       orgRepo,
		Authorization: authorization,
		Uri:           hfUri,
		DataType:      p.Job.Datatype,
		Etag:          etag,
		CacheResult:   make(chan error, 1),
	}
	taskParam.Context = bgCtx
	taskParam.ResponseChan = responseChan
	taskParam.Cancel = p.CancelFunc
	if err := p.DownloaderDao.FileDownload(offset, fileSize, false, taskParam); err != nil {
		return err
	}
	// Consumers may stop immediately, but the cache task must join the producer.
	consumeErr := p.result(bgCtx, responseChan)
	cacheErr := <-taskParam.CacheResult
	if cacheErr != nil && !errors.Is(cacheErr, context.Canceled) {
		return cacheErr
	}
	if consumeErr != nil {
		return consumeErr
	}
	if p.Ctx.Err() != nil {
		return p.Ctx.Err()
	}
	return cacheErr
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
			p.progressMu.Lock()
			p.StockSpeed = formatSpeed(delta, 1*time.Second) // 采样间隔 1 秒
			// 计算下载进度（百分比）
			process := float64(currentBytes) / float64(p.UsedStorage) * 100
			if process >= 100 {
				process = 99.9
			}
			p.StockProcess = float32(math.Round(process*10) / 10)
			p.progressMu.Unlock()
			lastBytes = currentBytes
		case <-ctx.Done():
			zap.S().Debug("speed ctx done")
			p.progressMu.Lock()
			p.StockSpeed = "0 B/s"
			p.progressMu.Unlock()
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
