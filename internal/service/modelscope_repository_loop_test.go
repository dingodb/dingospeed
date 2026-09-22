package service

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/pkg/config"
	"dingospeed/pkg/hfprojection"
	"dingospeed/pkg/repository"
)

func TestModelScopeRepositoryLoop(t *testing.T) {
	payload := "repository loop payload"
	oid := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			fmt.Fprintf(w, `{"Code":200,"Data":{"LatestCommitter":{"ShortId":"repo-head"},"Files":[{"Type":"blob","Path":"one.txt","Size":%d,"Sha256":%q,"Revision":"file-old"},{"Type":"blob","Path":"two.txt","Size":9,"Sha1":"other-object","Revision":"file-other"}]}}`, len(payload), oid)
			return
		}
		if r.URL.Query().Get("Revision") != "file-old" {
			t.Errorf("file fetch not pinned: %s", r.URL)
		}
		fmt.Fprint(w, payload)
	}))
	defer upstream.Close()
	msConfig(t, upstream.URL)
	// Exercise the shared repository/cache lifecycle with only an endpoint
	// override, as deployed configurations may omit every other provider option.
	config.SysConfig.Modelscope = config.Modelscope{OfficialBaseURL: upstream.URL}
	config.SysConfig.SetDefaults()
	e := msEngine()
	response := msRequest(e, "GET", "/file?repo=owner/model&revision=master&path=one.txt", "")
	if response.Code != 200 || response.Body.String() != payload {
		t.Fatalf("download: %d %s", response.Code, response.Body.String())
	}
	reader := hfprojection.Reader{Root: config.SysConfig.Repos(), Namespace: repository.ModelScope}
	catalog, err := reader.Catalog("models")
	if err != nil || len(catalog.Repos) != 1 {
		t.Fatalf("catalog: %+v %v", catalog, err)
	}
	revisions := catalog.Repos[0].Revisions
	if len(revisions) != 1 || revisions[0].Name != "master" || revisions[0].Commit != "repo-head" {
		t.Fatalf("file revisions became repository versions: %+v", revisions)
	}
	manifest, err := reader.Manifest("models", "owner/model", "master")
	if err != nil || !manifest.ManifestComplete || len(manifest.Files) != 2 {
		t.Fatalf("manifest: %+v %v", manifest, err)
	}
	f, body, err := reader.OpenFile("models", "owner/model", "master", "one.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	_ = body
	if _, _, err = reader.OpenFile("models", "owner/model", "master", "two.txt"); err == nil {
		t.Fatal("missing file reported readable")
	}
	admin := dao.NewCacheAdminDao(NewModelscopeService().fileDao)
	rows := admin.ListFiles("models", "modelscope/owner/model")
	if len(rows) != 2 {
		t.Fatalf("cache rows=%+v", rows)
	}
	var found bool
	for _, row := range rows {
		if row.Path == "one.txt" {
			found = row.Complete && row.CachedBytes == int64(len(payload)) && len(row.Revisions) == 1 && row.Revisions[0] == "master"
		} else if row.Complete || row.BlobExists {
			t.Fatal("uncached file reported complete")
		}
	}
	if !found {
		t.Fatalf("cached status wrong: %+v", rows)
	}
	hf, err := (hfprojection.Reader{Root: config.SysConfig.Repos()}).Catalog("models")
	if err != nil || len(hf.Repos) != 0 {
		t.Fatalf("namespace leak: %+v %v", hf, err)
	}
	results, err := admin.SoftDelete([]dao.DeleteItem{{Namespace: repository.ModelScope, RepoType: "models", Repo: "owner/model", OrgRepo: "modelscope/owner/model", Path: "one.txt", Sha: oid}})
	if err != nil || len(results) != 1 || results[0].Status != "deleted" {
		t.Fatalf("delete: %+v %v", results, err)
	}
	for _, row := range admin.ListFiles("models", "modelscope/owner/model") {
		if row.Path == "one.txt" {
			t.Fatal("deleted reference remains")
		}
	}
	if len(admin.ListOrphans("models", "modelscope/owner/model")) != 1 {
		t.Fatal("deleted blob missing from recycle bin")
	}
	// Metadata remains the upstream tree. Deleting cache does not rewrite its version.
	after, err := reader.Manifest("models", "owner/model", "master")
	if err != nil || after.Commit != "repo-head" || len(after.Files) != 2 {
		t.Fatalf("delete changed upstream tree: %+v %v", after, err)
	}

}

func TestModelScopeIncompleteSiblingDoesNotBlockSingleFile(t *testing.T) {
	payload := "valid file"
	oid := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repo/files") {
			fmt.Fprintf(w, `{"Code":200,"Data":{"LatestCommitter":{"ShortId":"repo-head"},"Files":[{"Type":"blob","Path":"one.txt","Size":%d,"Sha256":%q,"Revision":"file-old"},{"Type":"blob","Path":"other.txt","Size":9}]}}`, len(payload), oid)
			return
		}
		fmt.Fprint(w, payload)
	}))
	defer upstream.Close()
	msConfig(t, upstream.URL)
	response := msRequest(msEngine(), "GET", "/file?repo=owner/model&revision=master&path=one.txt", "")
	if response.Code != 200 || response.Body.String() != payload {
		t.Fatalf("unrelated sibling blocked valid file: %d %s", response.Code, response.Body.String())
	}
	catalog, err := (hfprojection.Reader{Root: config.SysConfig.Repos(), Namespace: repository.ModelScope}).Catalog("models")
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range catalog.Repos {
		if len(repo.Revisions) != 0 {
			t.Fatalf("incomplete snapshot was published: %+v", repo)
		}
	}
}
