package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/pkg/config"
)

// uploadLocalRepo 通过上传接口造出一个本地仓库，返回最新快照标识。
func uploadLocalRepo(t *testing.T, paths []string) (*MetaService, string) {
	t.Helper()
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{
		Server:   config.ServerConfig{Repos: t.TempDir()},
		Download: config.Download{BlockSize: 16},
		Upload:   config.Upload{Namespace: "dingo-local"},
	}
	config.SysConfig.Scheduler.PublicDomain = "http://speed.local"
	t.Cleanup(func() { config.SysConfig = oldConfig })

	baseData := data.NewBaseData()
	lockDao := dao.NewLockDao(baseData)
	fileDao := dao.NewFileDao(nil, baseData, lockDao)
	uploadDao := dao.NewUploadDao(fileDao, lockDao)

	var commit string
	for _, p := range paths {
		content := []byte("content of " + p)
		sum := sha256.Sum256(content)
		result, err := uploadDao.UploadWholeFile(dao.LocalUploadParam{
			RepoType: "models",
			Org:      "dingo-local",
			Repo:     "demo",
			Revision: "main",
			FilePath: p,
			Size:     int64(len(content)),
			Sha256:   hex.EncodeToString(sum[:]),
		}, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("upload %s failed: %v", p, err)
		}
		commit = result.Commit
	}
	return NewMetaService(fileDao, nil), commit
}

// 内部的仓库文件列表接口对本地仓库同样从清单派生。
func TestRepositoryFilesForLocalRepo(t *testing.T) {
	meta, commit := uploadLocalRepo(t, []string{"config.json", "subdir/tokenizer.json", "subdir/deep/extra.bin"})
	const orgRepo = "dingo-local/demo"

	root, err := meta.RepositoryFiles("models", orgRepo, commit, "")
	if err != nil {
		t.Fatalf("RepositoryFiles failed: %v", err)
	}
	if len(root) != 2 {
		t.Fatalf("expected one directory and one file at the root, got %+v", root)
	}
	if !root[0].IsDir || root[0].Name != "subdir" {
		t.Fatalf("expected the directory to sort first, got %+v", root[0])
	}
	if root[1].IsDir || root[1].Name != "config.json" || root[1].Size == 0 {
		t.Fatalf("unexpected root file entry: %+v", root[1])
	}
	wantLink := "http://speed.local/models/" + orgRepo + "/resolve/" + commit + "/config.json"
	if root[1].Link != wantLink {
		t.Fatalf("unexpected download link:\n got %s\nwant %s", root[1].Link, wantLink)
	}

	nested, err := meta.RepositoryFiles("models", orgRepo, commit, "subdir")
	if err != nil {
		t.Fatalf("RepositoryFiles for subdir failed: %v", err)
	}
	if len(nested) != 2 || !nested[0].IsDir || nested[0].Name != "deep" || nested[1].Name != "tokenizer.json" {
		t.Fatalf("unexpected subdir listing: %+v", nested)
	}

	if _, err = meta.RepositoryFiles("models", orgRepo, commit, "missing"); err == nil {
		t.Fatalf("expected an unknown directory to report file not exists")
	}
}

// 整仓文件树在去掉逐文件 paths-info 之后仍然完整。
func TestGetRepoTreeForLocalRepo(t *testing.T) {
	meta, _ := uploadLocalRepo(t, []string{"config.json", "subdir/tokenizer.json", "subdir/deep/extra.bin"})
	const orgRepo = "dingo-local/demo"

	tree, err := meta.GetRepoTree("models", orgRepo, "main", "", true, "")
	if err != nil {
		t.Fatalf("GetRepoTree failed: %v", err)
	}
	got := make([]string, 0, len(tree))
	for _, item := range tree {
		if item.Type != "file" {
			t.Fatalf("recursive tree should only contain files, got %+v", item)
		}
		got = append(got, item.Path)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 files in the recursive tree, got %v", got)
	}

	shallow, err := meta.GetRepoTree("models", orgRepo, "main", "", false, "")
	if err != nil {
		t.Fatalf("GetRepoTree (shallow) failed: %v", err)
	}
	if len(shallow) != 2 || shallow[0].Type != "directory" || shallow[0].Path != "subdir" {
		t.Fatalf("unexpected shallow tree: %+v", shallow)
	}
}
