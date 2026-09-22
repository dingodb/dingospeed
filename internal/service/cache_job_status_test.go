package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/service/task"
	"dingospeed/pkg/common"
	"dingospeed/pkg/repository"
)

func TestCacheJobShutdownRequiresServiceCancellation(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	for _, tc := range []struct {
		name        string
		stopService bool
		err         error
		want        string
	}{
		{"shutdown", true, fmt.Errorf("receive: %w", context.Canceled), "interrupted"},
		{"task cancellation", false, context.Canceled, "failed"},
		{"upstream failure", false, errors.New("upstream disconnected"), "failed"},
		{"failure during shutdown", true, errors.New("disk full"), "failed"},
		{"completed before shutdown", true, nil, "complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serviceCtx, stop := context.WithCancel(context.Background())
			defer stop()
			ctx, cancel := context.WithCancel(serviceCtx)
			defer cancel()
			key := repository.RepoKey{Namespace: repository.HuggingFace, RepoType: "models", Repo: "owner/model"}
			job := &task.PreheatCacheTask{CacheTask: task.CacheTask{TaskNo: 18, Ctx: ctx, LocalJob: true}, Sha: &dao.CommitHfSha{Sha: "pinned-commit"}, UsedStorage: 100}
			if err := trackCacheJob(job, key, serviceCtx); err != nil {
				t.Fatal(err)
			}
			if tc.stopService {
				stop()
			} else {
				cancel()
			}
			job.OnFinish(tc.err)
			// A new service can inspect the persisted result without resubmitting it.
			service := NewCacheJobService(nil, nil, nil, nil)
			defer service.cachePool.Close()
			for range 2 {
				status, err := service.CacheJobStatus(18)
				if err != nil || status.State != tc.want || status.Commit != "pinned-commit" || status.TotalBytes != 100 || !status.Local {
					t.Fatalf("restarted service: %+v %v", status, err)
				}
				if tc.want == "interrupted" && !strings.Contains(status.Error, "service shutdown; resume manually") {
					t.Fatalf("lost shutdown reason: %+v", status)
				}
				if tc.want == "failed" && status.Error != tc.err.Error() {
					t.Fatalf("lost original failure: %+v", status)
				}
				if _, running := service.cachePool.GetTask(18); running {
					t.Fatal("inspection must not resume downloads")
				}
			}
		})
	}
}

func TestCacheJobTerminalStatusSurvivesPoolRestart(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	for _, namespace := range []string{repository.HuggingFace, repository.ModelScope} {
		for _, failed := range []bool{false, true} {
			key := repository.RepoKey{Namespace: namespace, RepoType: "models", Repo: "owner/model"}
			job := &task.PreheatCacheTask{CacheTask: task.CacheTask{TaskNo: 17}, Sha: &dao.CommitHfSha{Sha: "repo-head"}, UsedStorage: 100}
			if err := trackCacheJob(job, key, context.Background()); err != nil {
				t.Fatal(err)
			}
			service := &CacheJobService{cachePool: common.NewPool(1, true)}
			status, err := service.CacheJobStatus(17)
			if err != nil || status.State != "running" {
				t.Fatalf("registered task: %+v %v", status, err)
			}
			want := "complete"
			var taskErr error
			if failed {
				want = "failed"
				taskErr = errors.New("upstream unavailable")
			}
			job.OnFinish(taskErr)
			status, err = service.CacheJobStatus(17)
			service.cachePool.Close()
			if err != nil || status.State != want || status.RepoKey != key || status.Commit != "repo-head" || status.TotalBytes != 100 {
				t.Fatalf("terminal task: %+v %v", status, err)
			}
			if failed && status.Error != taskErr.Error() {
				t.Fatalf("lost failure: %+v", status)
			}
		}
	}
}

func TestOrphanedCacheJobKeepsProgressWithoutResuming(t *testing.T) {
	msConfig(t, "http://unused.invalid")
	status := CacheJobStatus{
		ID: 19, RepoKey: repository.RepoKey{Namespace: repository.HuggingFace, RepoType: "models", Repo: "owner/model"},
		Commit: "pinned-commit", State: "running", CachedBytes: 50, TotalBytes: 100, Local: true,
	}
	if err := writeCacheJobStatus(status); err != nil {
		t.Fatal(err)
	}
	service := NewCacheJobService(nil, nil, nil, nil)
	defer service.cachePool.Close()
	for range 2 {
		got, err := service.CacheJobStatus(status.ID)
		if err != nil || got.State != "interrupted" || got.CachedBytes != 50 || got.Commit != status.Commit || !strings.Contains(got.Error, "resume manually") {
			t.Fatalf("orphaned task: %+v %v", got, err)
		}
		if _, running := service.cachePool.GetTask(int(status.ID)); running {
			t.Fatal("orphaned task was automatically resumed")
		}
	}
}
