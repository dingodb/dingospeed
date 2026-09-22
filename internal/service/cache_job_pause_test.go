package service

import (
	"context"
	"dingospeed/internal/dao"
	"dingospeed/internal/model/query"
	"dingospeed/internal/service/task"
	"dingospeed/pkg/repository"
	"errors"
	"testing"
)

func TestPauseWaitsForExecutionAndCancelUpgradesIntent(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	svc := NewCacheJobService(nil, nil, nil, nil)
	defer svc.cachePool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := repository.RepoKey{Namespace: repository.HuggingFace, RepoType: "models", Repo: "owner/model"}
	job := &task.PreheatCacheTask{CacheTask: task.CacheTask{TaskNo: 71, Ctx: ctx, CancelFunc: cancel, LocalJob: true}, Sha: &dao.CommitHfSha{Sha: "fixed"}, UsedStorage: 8}
	if err := trackCacheJob(job, key, context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.StopCacheJob(&query.JobStatusReq{Id: 71}); err != nil {
		t.Fatal(err)
	}
	first, err := svc.CacheJobStatus(71)
	if err != nil || first.State != "pausing" || ctx.Err() == nil {
		t.Fatalf("%+v %v", first, err)
	}
	if err := svc.ResumeCacheJob(nil, &query.ResumeCacheJobReq{Id: 71}); err == nil {
		t.Fatal("resume accepted before the execution exited")
	}
	if err := svc.StopCacheJob(&query.JobStatusReq{Id: 71, Intent: "cancel"}); err != nil {
		t.Fatal(err)
	}
	second, _ := svc.CacheJobStatus(71)
	if second.State != "canceling" || second.SnapshotVersion <= first.SnapshotVersion {
		t.Fatalf("%+v", second)
	}
	if err := svc.StopCacheJob(&query.JobStatusReq{Id: 71, Intent: "pause"}); err != nil {
		t.Fatal(err)
	}
	second, _ = svc.CacheJobStatus(71)
	if second.State != "canceling" {
		t.Fatal("cancel downgraded")
	}
	job.OnFinish(context.Canceled)
	done, err := svc.CacheJobStatus(71)
	if err != nil || done.State != "canceled" || done.Error != "" {
		t.Fatalf("%+v %v", done, err)
	}
	again, _ := svc.CacheJobStatus(71)
	if done != again {
		t.Fatal("same version returned a different snapshot")
	}
	if err := svc.ResumeCacheJob(nil, &query.ResumeCacheJobReq{Id: 71}); err == nil {
		t.Fatal("canceled job resumed")
	}
}

func TestRestartPersistsMonotonicSnapshots(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	svc := NewCacheJobService(nil, nil, nil, nil)
	defer svc.cachePool.Close()
	for i, state := range []string{"running", "pausing", "resuming", "canceling", "stopped"} {
		original := CacheJobStatus{ID: int64(90 + i), State: state, Generation: 3, StateVersion: 10, SnapshotVersion: 20}
		if err := writeCacheJobStatus(original); err != nil {
			t.Fatal(err)
		}
		got, err := svc.CacheJobStatus(original.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := "interrupted"
		if state == "canceling" {
			want = "canceled"
		}
		if state == "stopped" {
			want = "paused"
		}
		if got.State != want || got.Generation != 3 || got.StateVersion != 11 || got.SnapshotVersion != 21 {
			t.Fatalf("%+v", got)
		}
		again, _ := svc.CacheJobStatus(original.ID)
		if got != again {
			t.Fatal("restart reconciliation not durable")
		}
	}
}

func TestResumeConflictsWithOtherActiveJob(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	svc := NewCacheJobService(nil, nil, nil, nil)
	defer svc.cachePool.Close()
	key := repository.RepoKey{Namespace: repository.HuggingFace, RepoType: "models", Repo: "owner/model"}
	if err := writeCacheJobStatus(CacheJobStatus{ID: 81, RepoKey: key, State: "paused", Commit: "fixed", Local: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &task.PreheatCacheTask{CacheTask: task.CacheTask{TaskNo: 82, Ctx: ctx, CancelFunc: cancel, LocalJob: true}, Sha: &dao.CommitHfSha{Sha: "fixed"}}
	if err := trackCacheJob(job, key, context.Background()); err != nil {
		t.Fatal(err)
	}
	defer job.OnFinish(context.Canceled)
	err := svc.ResumeCacheJob(nil, &query.ResumeCacheJobReq{Id: 81})
	var conflict *CacheJobConflict
	if !errors.As(err, &conflict) || conflict.ActiveJobID != 82 || conflict.Code != "active_job_conflict" {
		t.Fatalf("%v", err)
	}
}
