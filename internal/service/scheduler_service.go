package service

import (
	"context"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/proto/manager"

	"go.uber.org/zap"
)

type SchedulerService struct {
	Client       manager.ManagerClient
	Ctx          context.Context
	schedulerDao *dao.SchedulerDao
	metaService  *MetaService
}

func NewSchedulerService(schedulerDao *dao.SchedulerDao, metaService *MetaService) *SchedulerService {
	return &SchedulerService{
		schedulerDao: schedulerDao,
		metaService:  metaService,
	}
}

func (s *SchedulerService) BindClient(client manager.ManagerClient) {
	s.Client = client
	s.schedulerDao.Client = client
}
func (s *SchedulerService) Register() {
	go s.ReportFileProcess()
	go s.ReconcilePublications()
	var previous config.Registration
	registered := false
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		r := config.SysConfig.Registration()
		if r != previous {
			registered = false
			previous = r
		}
		if !r.Enabled {
			registered = false
			runModeChange(consts.SchedulerModeStandalone)
			config.SysConfig.SetRegistrationStatus(r, "disabled", nil)
		} else {
			var err error
			if !registered {
				var response *manager.RegisterResponse
				response, err = s.schedulerDao.Register()
				if err == nil {
					config.SysConfig.SetSchedulerID(response.Id)
					registered = true
				}
			} else {
				err = s.schedulerDao.Heartbeat()
			}
			if err != nil {
				registered = false
				runModeChange(consts.SchedulerModeStandalone)
				config.SysConfig.SetRegistrationStatus(r, "error", err)
			} else if config.SysConfig.Registration() == r {
				config.SysConfig.SetRegistrationStatus(r, "connected", nil)
			}
		}
		select {
		case <-s.Ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *SchedulerService) Heartbeat() {
	ticker := time.NewTicker(time.Duration(config.SysConfig.Scheduler.Discovery.HeartbeatPeriod) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			err := s.schedulerDao.Heartbeat()
			if err != nil {
				zap.S().Errorf("speed:%s connect err.%v", config.SysConfig.Registration().NodeID, err)
				runModeChange(consts.SchedulerModeStandalone)
				break
			}
			runModeChange(consts.SchedulerModeCluster)
		case <-s.Ctx.Done():
			return
		}
	}
}

func (s *SchedulerService) ReportFileProcess() {
	for {
		select {
		case processParam, ok := <-data.GetFileProcessChan():
			if !ok {
				return
			}
			err := s.schedulerDao.ReportFileProcess(&manager.FileProcessRequest{
				ProcessId: processParam.ProcessId,
				StaPos:    processParam.StartPos,
				EndPos:    processParam.EndPos,
				Status:    processParam.Status,
			})
			if err != nil {
				zap.S().Errorf("ReportFileProcess err.%v", err)
				data.WriteLocalOperationChan(consts.OperationProcess, processParam) // write local
			}
		case <-s.Ctx.Done():
			return
		}
	}
}

func runModeChange(mode string) {
	if mode == consts.SchedulerModeStandalone {
		if config.SysConfig.GetSchedulerModel() == consts.SchedulerModeCluster {
			zap.S().Warnf("changed to standalone mode......")
			config.SysConfig.SetSchedulerModel(consts.SchedulerModeStandalone)
		}
	} else if mode == consts.SchedulerModeCluster {
		if config.SysConfig.GetSchedulerModel() == consts.SchedulerModeStandalone {
			zap.S().Warnf("changed to cluster mode......")
			config.SysConfig.SetSchedulerModel(consts.SchedulerModeCluster)
		}
	}
}
