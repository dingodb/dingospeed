package service

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/pkg/config"
)

// uploadLocalRepo 通过上传接口造出一个本地仓库，返回最新快照标识。
func uploadLocalRepo(t *testing.T, paths []string) (*MetaService, string) {
	return uploadLocalRepoRevision(t, "main", paths)
}

func uploadLocalRepoRevision(t *testing.T, revision string, paths []string) (*MetaService, string) {
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
			Revision: revision,
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

func TestMetaRevisionListsReadsAndDownloadsLikeMain(t *testing.T) {
	meta, commit := uploadLocalRepoRevision(t, "meta", []string{"README.md", "weights/model.bin"})
	const orgRepo = "dingo-local/demo"

	snapshot, err := meta.GetLocalSnapshot("models", orgRepo, "meta")
	if err != nil || snapshot.Commit != commit || len(snapshot.Files) != 2 {
		t.Fatalf("meta snapshot = %+v, err=%v", snapshot, err)
	}
	tree, err := meta.GetRepoTree("models", orgRepo, "meta", "", true, "")
	if err != nil || len(tree) != 2 {
		t.Fatalf("meta tree = %+v, err=%v", tree, err)
	}
	var output bytes.Buffer
	if err = meta.StreamLocalArchive("models", orgRepo, "meta", "demo", &output); err != nil {
		t.Fatalf("stream meta archive: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("open meta archive: %v", err)
	}
	if len(reader.File) != 2 {
		t.Fatalf("meta archive entries=%d", len(reader.File))
	}
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

func TestGetLocalSnapshotReturnsCommitAndCompleteManifest(t *testing.T) {
	meta, commit := uploadLocalRepo(t, []string{"config.json", "subdir/tokenizer.json"})
	snapshot, err := meta.GetLocalSnapshot("models", "dingo-local/demo", "main")
	if err != nil {
		t.Fatalf("GetLocalSnapshot failed: %v", err)
	}
	if snapshot.Commit != commit || len(snapshot.Files) != 2 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.Files[0].Path != "config.json" || snapshot.Files[0].Size == 0 || snapshot.Files[0].Sha256 == "" {
		t.Fatalf("unexpected first file: %+v", snapshot.Files[0])
	}
	if snapshot.Files[1].Path != "subdir/tokenizer.json" || snapshot.Files[1].Size == 0 || snapshot.Files[1].Sha256 == "" {
		t.Fatalf("unexpected second file: %+v", snapshot.Files[1])
	}
}

func TestStreamLocalArchiveMatchesExistingArchiveShape(t *testing.T) {
	meta, _ := uploadLocalRepo(t, []string{"config.json", "subdir/tokenizer.json"})
	var output bytes.Buffer
	if err := meta.StreamLocalArchive("models", "dingo-local/demo", "main", "demo", &output); err != nil {
		t.Fatalf("StreamLocalArchive failed: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("open archive failed: %v", err)
	}
	want := map[string]string{
		"demo/config.json":           "content of config.json",
		"demo/subdir/tokenizer.json": "content of subdir/tokenizer.json",
	}
	if len(reader.File) != len(want) {
		t.Fatalf("archive has %d entries, want %d", len(reader.File), len(want))
	}
	for _, file := range reader.File {
		if file.Method != zip.Store || !file.Modified.Equal(time.Unix(0, 0).UTC()) {
			t.Fatalf("unexpected header for %s: method=%d modified=%s", file.Name, file.Method, file.Modified)
		}
		body, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil || string(content) != want[file.Name] {
			t.Fatalf("unexpected content for %s: %q err=%v", file.Name, content, err)
		}
		delete(want, file.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing archive entries: %v", want)
	}
}
