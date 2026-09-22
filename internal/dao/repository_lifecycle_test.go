package dao

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"dingospeed/pkg/repository"
)

func TestRepositoryDeleteRecreatePurgesDataAndPreservesOtherRepositories(t *testing.T) {
	for _, withFiles := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "files"}[withFiles], func(t *testing.T) {
			u, root := newTestUploadDao(t)
			k := repository.RepoKey{RepoType: "models", Namespace: "dingo-local", Repo: "demo"}
			p := publishParam("main")
			if withFiles {
				body := []byte("original bytes")
				staged := deferredParam("a.bin", body)
				mustStage(t, u, staged, body)
				p.Files = []LocalManifestFile{manifestItem("a.bin", body)}
			}
			old := mustPublish(t, u, p)
			if _, err := u.fileDao.ReadLocalManifest(k.RepoType, k.ID(), old.Commit); err != nil {
				t.Fatal(err)
			}
			other := publishParam("main")
			other.Repo = "other"
			mustPublish(t, u, other)
			admin := NewCacheAdminDao(u.fileDao)
			if withFiles {
				items := []DeleteItem{{RepoType: k.RepoType, OrgRepo: k.ID(), Path: "a.bin", Sha: p.Files[0].Sha256}}
				if _, err := u.DeleteHostedRepository(k); err == nil {
					t.Fatal("finalization accepted live references")
				}
				rows, err := admin.SoftDelete(items)
				if err != nil || rows[0].Status == "failed" {
					t.Fatalf("unlink: %+v %v", rows, err)
				}
				rows, err = admin.PurgeOrphans(items)
				if err != nil || rows[0].Status == "failed" {
					t.Fatalf("purge: %+v %v", rows, err)
				}
			}
			deleted, err := u.DeleteHostedRepository(k)
			if err != nil || !deleted.Deleted {
				t.Fatalf("delete=%+v %v", deleted, err)
			}
			for _, path := range []string{k.APIRoot(root), k.FilesRoot(root), filepath.Join(root, "repository-trash")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("data retained at %s: %v", path, err)
				}
			}
			if _, err = repository.Read(root, k); !os.IsNotExist(err) {
				t.Fatalf("still active: %v", err)
			}
			if _, err = u.fileDao.ReadLocalManifest(k.RepoType, k.ID(), old.Commit); !os.IsNotExist(err) {
				t.Fatalf("old snapshot cache survived: %v", err)
			}
			if result, err := u.DeleteHostedRepository(k); err != nil || result.Deleted {
				t.Fatalf("retry=%+v %v", result, err)
			}
			p.Files = nil
			p.CreateOnly = true
			recreated := mustPublish(t, u, p)
			files, err := u.fileDao.ReadLocalManifest(k.RepoType, k.ID(), recreated.Commit)
			if err != nil || len(files) != 0 {
				t.Fatalf("recreated=%+v %v", files, err)
			}
			if _, err := u.PublishFiles(p); err == nil {
				t.Fatal("duplicate empty repository accepted")
			}
			if _, err := repository.Read(root, repository.RepoKey{RepoType: "models", Namespace: "dingo-local", Repo: "other"}); err != nil {
				t.Fatal(err)
			}
			if withFiles {
				body := []byte("original bytes")
				staged := deferredParam("a.bin", body)
				result, err := u.UploadWholeFile(staged, bytes.NewReader(body))
				if err != nil || result.BlobReused {
					t.Fatalf("deleted blob reused: %+v %v", result, err)
				}
			}
		})
	}
}

func TestRepositoryCreateOnlyHasOneWinner(t *testing.T) {
	u, _ := newTestUploadDao(t)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := publishParam("main")
			p.CreateOnly = true
			if _, err := u.PublishFiles(p); err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("winners=%d", winners.Load())
	}
}

func TestRepositoryDeleteFailureAndRemoteIsolation(t *testing.T) {
	u, root := newTestUploadDao(t)
	mustPublish(t, u, publishParam("main"))
	k := repository.RepoKey{RepoType: "models", Namespace: "dingo-local", Repo: "demo"}
	_ = root
	body := []byte("still live")
	mustStage(t, u, deferredParam("live.bin", body), body)
	p := publishParam("main")
	p.Files = []LocalManifestFile{manifestItem("live.bin", body)}
	mustPublish(t, u, p)
	if _, err := u.DeleteHostedRepository(k); err == nil {
		t.Fatal("live repository deleted without unlink")
	}
	if _, err := repository.Read(root, k); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{repository.HuggingFace, repository.ModelScope} {
		key := repository.RepoKey{RepoType: "models", Namespace: ns, Repo: "org/demo"}
		if err := repository.Register(root, repository.Remote(key)); err != nil {
			t.Fatal(err)
		}
		if _, err := u.DeleteHostedRepository(key); err == nil {
			t.Fatal("remote accepted")
		}
		if _, err := repository.Read(root, key); err != nil {
			t.Fatal(err)
		}
	}
}
