package dao

import (
	"context"

	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
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
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	r, err := s.Client.Register(ctx, &manager.RegisterRequest{
		InstanceId: config.SysConfig.Scheduler.Discovery.InstanceId,
		Host:       config.SysConfig.Scheduler.Discovery.Host,
		Port:       int32(config.SysConfig.Scheduler.Discovery.Port),
		Online:     config.SysConfig.Server.Online,
	})
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
		Id:         config.SysConfig.Id,
		InstanceId: config.SysConfig.Scheduler.Discovery.InstanceId,
		Online:     config.SysConfig.Server.Online}
	_, err := s.Client.Heartbeat(ctx, req)
	return err
}

func (s *SchedulerDao) SchedulerFile(req *manager.SchedulerFileRequest) (*manager.SchedulerFileResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	resp, err := s.Client.SchedulerFile(ctx, req)
	return resp, err
}

func (s *SchedulerDao) SyncFileProcess(req *manager.SyncFileProcessReq) error {
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
		zap.S().Errorf("ReportFileProcess fail.%v", ctx)
		return err
	}
	return nil
}

func (s *SchedulerDao) DeleteByEtagsAndFields(request *manager.DeleteByEtagsAndFieldsRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	_, err := s.Client.DeleteByEtagsAndFields(ctx, request)
	if err != nil {
		zap.S().Errorf("DeleteByEtagsAndFields fail.%v", ctx)
		return
	}
}

func (s *SchedulerDao) CreateCacheJob(request *manager.CreateCacheJobReq) (*manager.CreateCacheJobResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	resp, err := s.Client.CreateCacheJob(ctx, request)
	if err != nil {
		zap.S().Errorf("CreateCacheJob fail.%v", ctx)
		return nil, err
	}
	return resp, nil
}

func (s *SchedulerDao) UpdateCacheJobStatus(request *manager.UpdateCacheJobStatusReq) {
	ctx, cancel := context.WithTimeout(context.Background(), consts.RpcRequestTimeout)
	defer cancel()
	_, err := s.Client.UpdateCacheJobStatus(ctx, request)
	if err != nil {
		zap.S().Errorf("UpdateCacheJobStatus fail.%v", ctx)
		return
	}
}
