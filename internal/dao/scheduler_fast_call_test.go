package dao

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	pb "dingospeed/pkg/proto/manager"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

type stalledSchedulerClient struct {
	storageClient
	stall atomic.Bool
	calls atomic.Int32
}

func (c *stalledSchedulerClient) wait(ctx context.Context) error {
	c.calls.Add(1)
	if c.stall.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (c *stalledSchedulerClient) SchedulerFile(ctx context.Context, _ *pb.SchedulerFileRequest, _ ...grpc.CallOption) (*pb.SchedulerFileResponse, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return &pb.SchedulerFileResponse{ProcessId: 1}, nil
}

func (c *stalledSchedulerClient) SyncFileProcess(ctx context.Context, _ *pb.SyncFileProcessReq, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func TestSchedulerFastCallsTimeOutAndCoolDown(t *testing.T) {
	oldTimeout, oldCooldown := SchedulerFastTimeout, SchedulerFastCooldown
	defer func() { SchedulerFastTimeout, SchedulerFastCooldown = oldTimeout, oldCooldown }()
	SchedulerFastTimeout, SchedulerFastCooldown = 50*time.Millisecond, 200*time.Millisecond

	c := &stalledSchedulerClient{}
	c.stall.Store(true)
	d := &SchedulerDao{Client: c}
	req := &pb.SchedulerFileRequest{DataType: "models", Org: "huggingface", Repo: "Qwen/demo", Name: "weights.bin", Etag: "hash"}
	report := &pb.SyncFileProcessReq{FileProcessEntries: []*pb.FileProcessEntry{{DataType: "models", Org: "huggingface", Repo: "Qwen/demo"}}}

	start := time.Now()
	if _, err := d.SchedulerFile(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled scheduler: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s for a stalled scheduler", elapsed)
	}
	// During the cooldown neither lookups nor cache reports reach the scheduler.
	if _, err := d.SchedulerFile(req); !errors.Is(err, errSchedulerCooling) {
		t.Fatalf("lookup during cooldown: %v", err)
	}
	if err := d.ReportCachedFile(report); !errors.Is(err, errSchedulerCooling) {
		t.Fatalf("report during cooldown: %v", err)
	}
	if got := c.calls.Load(); got != 1 {
		t.Fatalf("scheduler called %d times", got)
	}

	c.stall.Store(false)
	time.Sleep(SchedulerFastCooldown)
	if _, err := d.SchedulerFile(req); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
	if err := d.ReportCachedFile(report); err != nil {
		t.Fatalf("report after cooldown: %v", err)
	}
}
