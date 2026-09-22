package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/ioutil"
	"sync"

	"dingospeed/internal/service"
	"dingospeed/pkg/config"
	"dingospeed/pkg/proto/manager"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type SchedulerServer struct {
	conn                  *registrationConnection
	schedulerService      *service.SchedulerService
	localOperationService *service.LocalOperationService
	sysService            *service.SysService
}

func NewSchedulerServer(schedulerService *service.SchedulerService, sysService *service.SysService, localOperationService *service.LocalOperationService) *SchedulerServer {
	return &SchedulerServer{
		schedulerService:      schedulerService,
		sysService:            sysService,
		localOperationService: localOperationService,
	}
}

func (s *SchedulerServer) Start(ctx context.Context) error {
	config.SysConfig.Registration()
	config.SysConfig.SetSchedulerModel("standalone")
	conn := &registrationConnection{}
	s.conn = conn
	client := manager.NewManagerClient(conn)
	s.schedulerService.BindClient(client)
	s.sysService.Client = client
	s.schedulerService.Ctx = ctx
	go s.schedulerService.Register()

	s.localOperationService.Ctx = ctx
	s.localOperationService.Initialize()
	return nil
}

// One stable client is shared by all services. Configuration changes replace only
// its transport, not service pointers or background consumers.
type registrationConnection struct {
	mu      sync.Mutex
	conn    *grpc.ClientConn
	address string
	closed  bool
}

func (c *registrationConnection) current() (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := config.SysConfig.Registration()
	if c.closed {
		return nil, errors.New("scheduler client stopped")
	}
	if !r.Enabled {
		if c.conn != nil {
			_ = c.conn.Close()
			c.conn = nil
		}
		return nil, errors.New("scheduler registration disabled")
	}
	if c.conn != nil && c.address == r.Address {
		return c.conn, nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	ssl := config.SysConfig.Server.Ssl
	creds := credential(ssl.CrtFile, ssl.KeyFile, ssl.CaFile, "zetyun.com")
	if creds == nil {
		return nil, errors.New("scheduler TLS certificate/key/CA unavailable")
	}
	conn, err := grpc.NewClient(r.Address, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	c.conn = conn
	c.address = r.Address
	return conn, nil
}
func (c *registrationConnection) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	conn, err := c.current()
	if err != nil {
		return err
	}
	return conn.Invoke(ctx, method, args, reply, opts...)
}
func (c *registrationConnection) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	conn, err := c.current()
	if err != nil {
		return nil, err
	}
	return conn.NewStream(ctx, desc, method, opts...)
}
func (c *registrationConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn != nil {
		return c.conn.Close()
	}
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
