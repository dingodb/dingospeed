package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"

	"dingospeed/internal/service"
	"dingospeed/pkg/config"
	"dingospeed/pkg/proto/manager"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type SchedulerServer struct {
	conn                *grpc.ClientConn
	schedulerService    *service.SchedulerService
	localProcessService *service.LocalProcessService
	sysService          *service.SysService
}

func NewSchedulerServer(schedulerService *service.SchedulerService, sysService *service.SysService, localProcessService *service.LocalProcessService) *SchedulerServer {
	return &SchedulerServer{
		schedulerService:    schedulerService,
		sysService:          sysService,
		localProcessService: localProcessService,
	}
}

func (s *SchedulerServer) Start(ctx context.Context) error {
	if !config.SysConfig.IsCluster() {
		return nil
	}
	ssl := config.SysConfig.Server.Ssl
	creds := credential(ssl.CrtFile, ssl.KeyFile, ssl.CaFile, "zetyun.com")
	conn, err := grpc.NewClient(config.SysConfig.Scheduler.Addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		zap.S().Errorf("连接失败: %v", err)
		return err
	}
	s.conn = conn
	client := manager.NewManagerClient(conn)
	s.schedulerService.Client = client
	s.sysService.Client = client
	s.schedulerService.Ctx = ctx
	s.schedulerService.Register()

	s.localProcessService.Ctx = ctx
	s.localProcessService.Run()
	return nil
}

func credential(crtFile, keyFile, caFile, svcName string) credentials.TransportCredentials {
	cert, err := tls.LoadX509KeyPair(crtFile, keyFile)
	if err != nil {
		zap.S().Errorf("could not load client key pair: %v", err)
		return nil
	}
	certPool := x509.NewCertPool()
	ca, err := ioutil.ReadFile(caFile)
	if err != nil {
		zap.S().Errorf("could not read ca certificate: %v", err)
		return nil
	}
	if ok := certPool.AppendCertsFromPEM(ca); !ok {
		zap.S().Errorf("failed to append ca certs")
		return nil
	}
	conf := &tls.Config{
		ServerName:   svcName,
		Certificates: []tls.Certificate{cert},
		RootCAs:      certPool,
	}
	return credentials.NewTLS(conf)
}

func (s *SchedulerServer) Stop(ctx context.Context) error {
	if s.conn != nil {
		zap.S().Infof("[GRPC] client shutdown.")
		err := s.conn.Close()
		if err != nil {
			zap.S().Errorf("conn close fail.%v", err)
			return err
		}
	}
	return nil
}
