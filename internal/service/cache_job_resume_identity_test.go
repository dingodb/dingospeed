package service

import (
	"context"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/service/task"
	"dingospeed/pkg/config"
	"dingospeed/pkg/proto/manager"
	"dingospeed/pkg/repository"
)

func TestResumeRetainsOriginalTaskAuthority(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	svc := &CacheJobService{}
	old := CacheJobStatus{Local: true}
	if err := config.SysConfig.SaveRegistration(config.Registration{Enabled: true, NodeID: "node-a", Address: "scheduler:19091", Host: "speed", Port: 8090}); err != nil {
		t.Fatal(err)
	}
	// No scheduler client: old local work must remain independent even offline.
	for _, requested := range []string{"", "node-a"} {
		local, id, err := svc.resumeCacheJobIdentity(old, requested)
		if err != nil || !local || id != "" {
			t.Fatalf("local authority lost: %v %q %v", local, id, err)
		}
	}
	if _, _, err := svc.resumeCacheJobIdentity(old, "node-b"); err == nil {
		t.Fatal("wrong explicit node accepted")
	}
	cluster := CacheJobStatus{InstanceID: "node-a"}
	if _, _, err := svc.resumeCacheJobIdentity(cluster, ""); err == nil {
		t.Fatal("cluster work accepted without scheduler")
	}
	svc.schedulerDao = &dao.SchedulerDao{Client: manager.NewManagerClient(nil)}
	if local, id, err := svc.resumeCacheJobIdentity(cluster, ""); err != nil || local || id != "node-a" {
		t.Fatalf("cluster identity changed: %v %q %v", local, id, err)
	}
	cluster.InstanceID = "node-b"
	if _, _, err := svc.resumeCacheJobIdentity(cluster, ""); err == nil {
		t.Fatal("foreign cluster job accepted")
	}
}

func TestActiveCacheExclusionAcrossModes(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	svc := NewCacheJobService(nil, nil, nil, nil)
	defer svc.cachePool.Close()
	key := repository.RepoKey{Namespace: repository.HuggingFace, RepoType: "models", Repo: "owner/model"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &task.PreheatCacheTask{CacheTask: task.CacheTask{TaskNo: 981, Ctx: ctx, CancelFunc: cancel, LocalJob: true}, Sha: &dao.CommitHfSha{Sha: "fixed"}}
	if err := trackCacheJob(job, key, context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.reusableCacheJob(key, job.Sha, false, "node-a")
	if err != nil || got == nil || got.ID != 981 {
		t.Fatalf("cross-mode writer not excluded: %+v %v", got, err)
	}
	job.OnFinish(nil)
	status, err := svc.CacheJobStatus(981)
	if err != nil || status.State != "complete" || !status.Local || status.InstanceID != "" {
		t.Fatalf("local completion changed authority: %+v %v", status, err)
	}
}
