package dao

import (
	"context"
	"dingospeed/pkg/config"
	"dingospeed/pkg/dependency"
	"dingospeed/pkg/proto/manager"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"testing"
	"time"
)

type observationClient struct {
	manager.ManagerClient
	err      error
	response *manager.RegisterResponse
	calls    int
}

func (c *observationClient) Register(context.Context, *manager.RegisterRequest, ...grpc.CallOption) (*manager.RegisterResponse, error) {
	c.calls++
	return c.response, c.err
}
func (c *observationClient) Heartbeat(context.Context, *manager.HeartbeatRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	c.calls++
	return nil, c.err
}

func TestSchedulerObservationPreservesRPCResults(t *testing.T) {
	old := config.SysConfig
	config.SysConfig = &config.Config{}
	defer func() { config.SysConfig = old }()
	client := &observationClient{response: &manager.RegisterResponse{}}
	dao := NewSchedulerDao()
	dao.Client = client
	response, err := dao.Register()
	if err != nil || response != client.response || client.calls != 1 {
		t.Fatal("successful registration changed")
	}
	want := errors.New("simulated unavailable")
	client.err = want
	_, err = dao.Register()
	if err != want || client.calls != 2 {
		t.Fatal("failure changed or retried")
	}
	if err := dao.Heartbeat(); err != want || client.calls != 3 {
		t.Fatal("heartbeat failure changed or retried")
	}
	if !dependency.Default.Snapshot(dependency.Scheduler, time.Now()).Unresolved {
		t.Fatal("RPC failure was not observed")
	}
}
