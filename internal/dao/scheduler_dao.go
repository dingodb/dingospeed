package dao

import (
	"context"
	"dingospeed/pkg/repository"
	"errors"
	"google.golang.org/protobuf/proto"
	"sync/atomic"
	"time"

	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/dependency"
	"dingospeed/pkg/proto/manager"

	"go.uber.org/zap"
)

// 下载路径上的调度调用（选取对等节点、上报文件已缓存）失败时都有退路：
// 回源下载，或写入本地队列稍后重放。短超时加冷却期，避免调度器或其数据库
// 卡住时，每个文件都要等满 RpcRequestTimeout，并在调度器的写锁上排队。
var (
	SchedulerFastTimeout  = 5 * time.Second
	SchedulerFastCooldown = 30 * time.Second
)

var errSchedulerCooling = errors.New("scheduler calls paused after a recent failure")

type SchedulerDao struct {
	Client        manager.ManagerClient
	fastSkipUntil atomic.Int64
}

func (s *SchedulerDao) fastCall(call func(context.Context) error) error {
	if time.Now().UnixNano() < s.fastSkipUntil.Load() {
		return errSchedulerCooling
	}
	ctx, cancel := context.WithTimeout(context.Background(), SchedulerFastTimeout)
	defer cancel()
	if err := call(ctx); err != nil {
		s.fastSkipUntil.Store(time.Now().Add(SchedulerFastCooldown).UnixNano())
		return err
	}
	return nil
}

func NewSchedulerDao() *SchedulerDao {
	return &SchedulerDao{}
}

func (s *SchedulerDao) Register() (*manager.RegisterResponse, error) {
	managementURL, downloadURL := config.SysConfig.RegistrationEndpoints()
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	r, err := s.Client.Register(ctx, &manager.RegisterRequest{
		InstanceId:    config.SysConfig.Registration().NodeID,
		Host:          config.SysConfig.Registration().Host,
		Port:          int32(config.SysConfig.Registration().Port),
		Online:        config.SysConfig.Server.Online,
		ManagementUrl: managementURL,
		DownloadUrl:   downloadURL,
	})
	dependency.Default.Observe(dependency.Scheduler, err == nil, time.Now())
	if err != nil {
		zap.S().Errorf("speed register fail.%v", err)
		return nil, err
	}
	return r, nil
}

func (s *SchedulerDao) Heartbeat() error {
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	req := &manager.HeartbeatRequest{
		Id:         config.SysConfig.SchedulerID(),
		InstanceId: config.SysConfig.Registration().NodeID,
		Online:     config.SysConfig.Server.Online,
		Health:     heartbeatHealth(time.Now())}
	_, err := s.Client.Heartbeat(ctx, req)
	dependency.Default.Observe(dependency.Scheduler, err == nil, time.Now())
	return err
}

func (s *SchedulerDao) SchedulerFile(req *manager.SchedulerFileRequest) (*manager.SchedulerFileResponse, error) {
	req = proto.Clone(req).(*manager.SchedulerFileRequest)
	if req.Org != "" || req.Repo != "" {
		org, repo, err := (repository.RepoKey{Namespace: req.Org, RepoType: req.DataType, Repo: req.Repo}).Storage()
		if err != nil {
			return nil, err
		}
		req.Org, req.Repo = org, repo
	}

	var resp *manager.SchedulerFileResponse
	err := s.fastCall(func(ctx context.Context) (err error) {
		resp, err = s.Client.SchedulerFile(ctx, req)
		return err
	})
	return resp, err
}

func (s *SchedulerDao) SyncFileProcess(req *manager.SyncFileProcessReq) error {
	req, err := storageFileProcess(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	_, err = s.Client.SyncFileProcess(ctx, req)
	return err
}

// ReportCachedFile 在下载路径上同步执行，失败由调用方写入本地队列重放。
func (s *SchedulerDao) ReportCachedFile(req *manager.SyncFileProcessReq) error {
	req, err := storageFileProcess(req)
	if err != nil {
		return err
	}
	return s.fastCall(func(ctx context.Context) error {
		_, err := s.Client.SyncFileProcess(ctx, req)
		return err
	})
}

func storageFileProcess(req *manager.SyncFileProcessReq) (*manager.SyncFileProcessReq, error) {
	req = proto.Clone(req).(*manager.SyncFileProcessReq)
	for _, e := range req.FileProcessEntries {
		org, repo, err := (repository.RepoKey{Namespace: e.Org, RepoType: e.DataType, Repo: e.Repo}).Storage()
		if err != nil {
			return nil, err
		}
		e.Org, e.Repo = org, repo
	}
	return req, nil
}

func (s *SchedulerDao) ReportFileProcess(request *manager.FileProcessRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	_, err := s.Client.ReportFileProcess(ctx, request)
	if err != nil {
		return err
	}
	return nil
}

func (s *SchedulerDao) DeleteByEtagsAndFields(request *manager.DeleteByEtagsAndFieldsRequest) {
	request = proto.Clone(request).(*manager.DeleteByEtagsAndFieldsRequest)
	if request.Org != "" || request.Repo != "" {
		org, repo, err := (repository.RepoKey{Namespace: request.Org, RepoType: request.Datatype, Repo: request.Repo}).Storage()
		if err != nil {
			return
		}
		request.Org, request.Repo = org, repo
	}

	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	_, err := s.Client.DeleteByEtagsAndFields(ctx, request)
	if err != nil {
		zap.S().Errorf("DeleteByEtagsAndFields fail.%v", err)
		return
	}
}

func (s *SchedulerDao) CreateCacheJob(request *manager.CreateCacheJobReq) (*manager.CreateCacheJobResp, error) {
	request = proto.Clone(request).(*manager.CreateCacheJobReq)
	if request.Org != "" || request.Repo != "" {
		org, repo, err := (repository.RepoKey{Namespace: request.Org, RepoType: request.Datatype, Repo: request.Repo}).Storage()
		if err != nil {
			return nil, err
		}
		request.Org, request.Repo = org, repo
	}

	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	resp, err := s.Client.CreateCacheJob(ctx, request)
	if err != nil {
		zap.S().Errorf("CreateCacheJob fail.%v", err)
		return nil, err
	}
	return resp, nil
}

func (s *SchedulerDao) UpdateCacheJobStatus(request *manager.UpdateCacheJobStatusReq) error {
	request = proto.Clone(request).(*manager.UpdateCacheJobStatusReq)
	if request.Org != "" || request.Repo != "" {
		org, repo, err := (repository.RepoKey{Namespace: request.Org, RepoType: "models", Repo: request.Repo}).Storage()
		if err != nil {
			return err
		}
		request.Org, request.Repo = org, repo
	}

	zap.S().Infof("update status cacheJobId:%d, status:%d, %s", request.Id, request.Status, request.ErrorMsg)
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	_, err := s.Client.UpdateCacheJobStatus(ctx, request)
	return err
}

func (s *SchedulerDao) ExecUpdateCacheJobStatus(jobId int, status int32, instanceId, org, repo, errorMsg string, process float32) {
	request := &manager.UpdateCacheJobStatusReq{
		Id:         int64(jobId),
		InstanceId: instanceId,
		Status:     status,
		ErrorMsg:   errorMsg,
		Org:        org,
		Repo:       repo,
		Process:    process,
	}
	if err := s.UpdateCacheJobStatus(request); err != nil {
		data.WriteLocalOperationChan(consts.OperationPreheat, request)
	}
}

func (s *SchedulerDao) UpdateRepositoryMountStatus(request *manager.UpdateRepositoryMountStatusReq) error {
	zap.S().Infof("updateRepositoryMountStatus id:%d, status:%d, %s", request.Id, request.Status, request.ErrorMsg)
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	_, err := s.Client.UpdateRepositoryMountStatus(ctx, request)
	return err
}

func (s *SchedulerDao) ExecUpdateRepositoryMountStatus(jobId int, status int32, errorMsg string) {
	request := &manager.UpdateRepositoryMountStatusReq{
		Id:       int64(jobId),
		Status:   status,
		ErrorMsg: errorMsg,
	}
	if err := s.UpdateRepositoryMountStatus(request); err != nil {
		data.WriteLocalOperationChan(consts.OperationMount, request)
	}

}
