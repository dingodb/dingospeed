package server

import (
	"context"

	"dingospeed/internal/dao"
	"dingospeed/pkg/config"

	"go.uber.org/zap"
)

type UploadCleanupServer struct {
	uploadDao *dao.UploadDao
}

func NewUploadCleanupServer(uploadDao *dao.UploadDao) *UploadCleanupServer {
	return &UploadCleanupServer{uploadDao: uploadDao}
}

func (s *UploadCleanupServer) Start(ctx context.Context) error {
	zap.S().Infof("[UPLOAD-CLEANUP] staged upload cleanup interval=%s retention=%s",
		config.SysConfig.GetUploadStagingCleanupInterval(), config.SysConfig.GetUploadStagingRetention())
	go s.uploadDao.RunStagedUploadCleanup(ctx)
	return nil
}

func (s *UploadCleanupServer) Stop(ctx context.Context) error {
	zap.S().Infof("[UPLOAD-CLEANUP] server shutdown.")
	return nil
}
