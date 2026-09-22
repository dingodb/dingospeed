package dao

import (
	"context"
	"errors"
	"testing"

	"dingospeed/internal/data"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	pb "dingospeed/pkg/proto/manager"
	"dingospeed/pkg/repository"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

type reconcileClient struct {
	storageClient
	fail bool
}

func (c *reconcileClient) SyncFileProcess(ctx context.Context, r *pb.SyncFileProcessReq, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.sync = r
	if c.fail {
		return nil, errors.New("scheduler unavailable")
	}
	return &emptypb.Empty{}, nil
}

func TestCacheReconcileIdentityAndRetry(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	config.SysConfig = &config.Config{}
	config.SysConfig.Scheduler.Mode = consts.SchedulerModeCluster
	config.SysConfig.Scheduler.OriginMode = consts.SchedulerModeCluster
	config.SysConfig.Scheduler.Discovery.InstanceId = "node-2"
	data.NewBaseData()
	c := &reconcileClient{}
	d := NewDownloaderDao(&SchedulerDao{Client: c})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &downloader.TaskParam{Context: ctx, DataType: "models", FileName: "weights.bin", Etag: "hash", FileSize: 7,
		RepoKey: repository.RepoKey{Namespace: "huggingface", RepoType: "models", Repo: "Qwen/demo"}}
	for i := 0; i < 2; i++ {
		d.reconcileCachedFile(p)
		e := c.sync.FileProcessEntries[0]
		if e.Org != "Qwen" || e.Repo != "demo" || e.InstanceId != "node-2" || e.ProcessId != 0 || e.StartPos != 7 || e.EndPos != 7 || e.Status != consts.StatusDownloaded {
			t.Fatalf("incorrect reconciliation: %v", e)
		}
	}
	c.fail = true
	d.reconcileCachedFile(p)
	select {
	case op := <-data.GetLocalOperationChan():
		if op.Type != consts.OperationProcess || op.IdentityVersion != 2 {
			t.Fatalf("wrong retry: %+v", op)
		}
	default:
		t.Fatal("failed reconciliation was not queued for retry")
	}
}
