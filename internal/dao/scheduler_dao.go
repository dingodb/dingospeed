package dao

import (
	"context"
	"dingospeed/pkg/repository"
	"google.golang.org/protobuf/proto"
	"time"

	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/dependency"
	"dingospeed/pkg/proto/manager"

	"go.uber.org/zap"
)

type SchedulerDao struct {
	Client manager.ManagerClient
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

	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	resp, err := s.Client.SchedulerFile(ctx, req)
	return resp, err
}

func (s *SchedulerDao) SyncFileProcess(req *manager.SyncFileProcessReq) error {
	req = proto.Clone(req).(*manager.SyncFileProcessReq)
	for _, e := range req.FileProcessEntries {
		org, repo, err := (repository.RepoKey{Namespace: e.Org, RepoType: e.DataType, Repo: e.Repo}).Storage()
		if err != nil {
			return err
		}
		e.Org, e.Repo = org, repo
	}

	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	_, err := s.Client.SyncFileProcess(ctx, req)
	return err
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
