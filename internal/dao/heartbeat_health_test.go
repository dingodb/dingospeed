package dao

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/dependency"
	pb "dingospeed/pkg/proto/manager"
	"dingospeed/pkg/util"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type healthClient struct {
	pb.ManagerClient
	received *pb.HeartbeatRequest
	err      error
}

func (c *healthClient) Heartbeat(_ context.Context, r *pb.HeartbeatRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	encoded, err := proto.Marshal(r)
	if err != nil {
		return nil, err
	}
	c.received = &pb.HeartbeatRequest{}
	if err = proto.Unmarshal(encoded, c.received); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, c.err
}

func TestHeartbeatCarriesMemoryHealthWithoutStorage(t *testing.T) {
	oldConfig, oldMonitor := config.SysConfig, dependency.Default
	t.Cleanup(func() { config.SysConfig = oldConfig; dependency.Default = oldMonitor })
	config.SysConfig = &config.Config{}
	config.SysConfig.Id = 7
	config.SysConfig.Scheduler.Discovery.HeartbeatPeriod = 5
	config.SysConfig.Scheduler.Discovery.InstanceId = "hd-test"
	config.SysConfig.Server.Repos = "\x00-inaccessible"
	dependency.Default = &dependency.Monitor{}
	client := &healthClient{err: errors.New("communication unavailable")}
	dao := NewSchedulerDao()
	dao.Client = client
	util.ObserveFileAccessFailure("read", &os.PathError{Op: "read", Path: "unavailable-cache", Err: os.ErrPermission})
	if err := dao.Heartbeat(); err != client.err {
		t.Fatal("heartbeat error semantics changed")
	}
	client.err = nil
	if err := dao.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	h := client.received.Health
	if client.received.Id != 7 || h == nil || h.Version != 1 || !h.Capabilities[0].Unresolved || h.Capabilities[1].State != 0 {
		t.Fatalf("missing health: %v", client.received)
	}
	found := false
	for _, e := range h.Errors {
		if e.Operation == "read" && e.Kind == "permission_denied" && e.Count > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("classified counter not transported")
	}
	if next := heartbeatHealth(time.Now()); next.ProcessId != h.ProcessId || next.StartedAt != h.StartedAt {
		t.Fatal("process identity changes every heartbeat")
	}
	if len(h.Errors) > 16 || len(h.Capabilities) != 2 {
		t.Fatal("unbounded report")
	}
	before := time.Now()
	dependency.Default.Observe(dependency.MetadataRead, false, before.Add(time.Second))
	next := heartbeatHealth(before)
	if next.CollectedAt < next.Capabilities[0].LastObservation {
		t.Fatal("concurrent observation produced invalid report timestamp")
	}
}
