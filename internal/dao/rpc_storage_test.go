package dao

import (
	"context"
	pb "dingospeed/pkg/proto/manager"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"testing"
)

type storageClient struct {
	pb.ManagerClient
	file *pb.SchedulerFileRequest
	sync *pb.SyncFileProcessReq
}

func (c *storageClient) SchedulerFile(_ context.Context, r *pb.SchedulerFileRequest, _ ...grpc.CallOption) (*pb.SchedulerFileResponse, error) {
	c.file = r
	return &pb.SchedulerFileResponse{ProcessId: 41}, nil
}
func (c *storageClient) SyncFileProcess(_ context.Context, r *pb.SyncFileProcessReq, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	c.sync = r
	return &emptypb.Empty{}, nil
}
func TestRPCStorageBoundaryDoesNotMutateRetryMessage(t *testing.T) {
	c := &storageClient{}
	d := &SchedulerDao{Client: c}
	for _, ns := range []string{"huggingface", "datacanvas", "alice"} {
		repo := "demo"
		org := "dingo-local/" + ns
		if ns == "huggingface" {
			repo = "Qwen/demo"
			org = "Qwen"
		}
		req := &pb.SchedulerFileRequest{DataType: "models", Org: ns, Repo: repo, Name: "weights.bin", Etag: "hash"}
		for i := 0; i < 2; i++ {
			if _, e := d.SchedulerFile(req); e != nil {
				t.Fatal(e)
			}
			if c.file.Org != org || c.file.Repo != "demo" {
				t.Fatalf("wrong SQL coordinates: %v", c.file)
			}
			if req.Org != ns || req.Repo != repo {
				t.Fatal("retry message mutated")
			}
		}
		sync := &pb.SyncFileProcessReq{FileProcessEntries: []*pb.FileProcessEntry{{DataType: "models", Org: ns, Repo: repo}}}
		if e := d.SyncFileProcess(sync); e != nil {
			t.Fatal(e)
		}
		if c.sync.FileProcessEntries[0].Org != org || sync.FileProcessEntries[0].Org != ns {
			t.Fatal("wrong sync boundary")
		}
	}
}
