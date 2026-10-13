package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
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
