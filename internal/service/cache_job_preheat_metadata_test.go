package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/model/query"
	"dingospeed/pkg/app"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/repository"

	"github.com/labstack/echo/v4"
)

func TestPreheatMetadataBatchesPathsInfo(t *testing.T) {
	const files = 200
	for _, dropOne := range []bool{false, true} {
		t.Run(fmt.Sprint("dropOne=", dropOne), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/revision/"):
					siblings := make([]string, files)
					for i := range siblings {
						siblings[i] = fmt.Sprintf(`{"rfilename":"part-%03d.parquet"}`, i)
					}
					fmt.Fprintf(w, `{"sha":"commit-1","siblings":[%s]}`, strings.Join(siblings, ","))
				case strings.Contains(r.URL.Path, "/paths-info/"):
					calls.Add(1)
					var req struct {
						Paths []string `json:"paths"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					out := []string{}
					for i, path := range req.Paths {
						if dropOne && i == 0 {
							continue
						}
						out = append(out, fmt.Sprintf(`{"type":"file","path":%q,"oid":"oid","size":3}`, path))
					}
					fmt.Fprintf(w, "[%s]", strings.Join(out, ","))
				default:
					t.Errorf("unexpected upstream %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer upstream.Close()
			msConfig(t, upstream.URL)
			config.SysConfig.Server.HfScheme = "http"
			config.SysConfig.Server.HfNetLoc = strings.TrimPrefix(upstream.URL, "http://")
			base := data.NewBaseData()
			locks := dao.NewLockDao(base)
			downloads := dao.NewDownloaderDao(nil)
			fileDao := dao.NewFileDao(downloads, base, locks)
			svc := NewCacheJobService(fileDao, dao.NewMetaDao(fileDao, locks, base), downloads, nil)
			defer svc.cachePool.Close()

			meta, err := svc.preheatMetadata(context.Background(), repository.RepoKey{Namespace: "huggingface", RepoType: "datasets", Repo: "owner/data"}, "main", "")
			if dropOne {
				if err == nil {
					t.Fatal("incomplete paths-info accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			} else if meta.UsedStorage != 3*200 {
				t.Fatalf("usedStorage %d", meta.UsedStorage)
			}
			if got := calls.Load(); dropOne && got != 1 || !dropOne && got != 2 {
				t.Fatalf("paths-info calls %d", got)
			}
		})
	}
}

func TestPreheatSnapshotSizeUsesOneBlobsRequest(t *testing.T) {
	const n = 1000
	var blobs, pathsInfo atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			siblings := make([]string, n)
			for i := range siblings {
				siblings[i] = fmt.Sprintf(`{"rfilename":"part-%04d.jsonl","size":5}`, i)
				if r.URL.Query().Get("blobs") != "true" {
					siblings[i] = fmt.Sprintf(`{"rfilename":"part-%04d.jsonl"}`, i)
				}
			}
			if r.URL.Query().Get("blobs") == "true" {
				blobs.Add(1)
			}
			fmt.Fprintf(w, `{"sha":"commit-1","siblings":[%s]}`, strings.Join(siblings, ","))
		case strings.Contains(r.URL.Path, "/paths-info/"):
			pathsInfo.Add(1)
			w.WriteHeader(500)
		default:
			t.Errorf("unexpected upstream %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	svc := preheatTestService(t, upstream.URL)
	meta, err := svc.preheatMetadata(context.Background(), repository.RepoKey{Namespace: "huggingface", RepoType: "datasets", Repo: "owner/data"}, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta.UsedStorage != 5*n || blobs.Load() != 1 || pathsInfo.Load() != 0 {
		t.Fatalf("usedStorage %d, blobs %d, paths-info %d", meta.UsedStorage, blobs.Load(), pathsInfo.Load())
	}
}

func TestCreatePreheatReturnsBeforeSnapshotIsSized(t *testing.T) {
	gate := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(gate) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/") && r.URL.Query().Get("blobs") == "true":
			// Measuring the snapshot is slow upstream; create must not wait for it.
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, `{"sha":"commit-1","siblings":[{"rfilename":"a.bin","size":3},{"rfilename":"b.bin","size":5}]}`)
		case strings.Contains(r.URL.Path, "/revision/"):
			fmt.Fprint(w, `{"sha":"commit-1","usedStorage":999,"siblings":[{"rfilename":"a.bin"},{"rfilename":"b.bin"}]}`)
		case strings.Contains(r.URL.Path, "/paths-info/"):
			var req struct {
				Paths []string `json:"paths"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			size := map[string]int{"a.bin": 3, "b.bin": 5}[req.Paths[0]]
			fmt.Fprintf(w, `[{"type":"file","path":%q,"oid":"oid-%s","size":%d}]`, req.Paths[0], req.Paths[0], size)
		case strings.Contains(r.URL.Path, "/resolve/"):
			body := "abc"
			if strings.HasSuffix(r.URL.Path, "b.bin") {
				body = "abcde"
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			fmt.Fprint(w, body)
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	svc := preheatTestService(t, upstream.URL)
	config.SysConfig.Scheduler.Mode = "standalone"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/api/cacheJob/create", nil).WithContext(app.NewContext(ctx, app.New(app.Context(ctx))))

	start := time.Now()
	result, err := svc.CreateCacheJobResult(echo.New().NewContext(req, httptest.NewRecorder()), &query.CreateCacheJobReq{Type: consts.CacheTypePreheat, Namespace: "huggingface", Datatype: "datasets", Repo: "owner/data"})
	if err != nil || result.Disposition != "created" {
		t.Fatalf("create: %+v %v", result, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("create waited %s for sizing", elapsed)
	}
	release.Do(func() { close(gate) })
	waitCacheJob(t, svc, result.ID, "complete")
	status, err := svc.CacheJobStatus(result.ID)
	if err != nil || status.TotalBytes != 8 {
		t.Fatalf("measured size not recorded: %+v %v", status, err)
	}
}

func preheatTestService(t *testing.T, upstreamURL string) *CacheJobService {
	t.Helper()
	msConfig(t, upstreamURL)
	config.SysConfig.Server.HfScheme = "http"
	config.SysConfig.Server.HfNetLoc = strings.TrimPrefix(upstreamURL, "http://")
	base := data.NewBaseData()
	locks := dao.NewLockDao(base)
	downloads := dao.NewDownloaderDao(nil)
	fileDao := dao.NewFileDao(downloads, base, locks)
	svc := NewCacheJobService(fileDao, dao.NewMetaDao(fileDao, locks, base), downloads, nil)
	t.Cleanup(svc.cachePool.Close)
	return svc
}
