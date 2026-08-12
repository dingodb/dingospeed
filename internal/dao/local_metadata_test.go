package dao

import (
	"fmt"
	"testing"

	"dingospeed/internal/data"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"

	"github.com/bytedance/sonic"
)

func TestWriteMetaIncludesLocalRepoID(t *testing.T) {
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{
		Server: config.ServerConfig{Repos: t.TempDir()},
		Upload: config.Upload{Namespace: "dingo-local"},
	}
	defer func() { config.SysConfig = oldConfig }()

	baseData := data.NewBaseData()
	lockDao := NewLockDao(baseData)
	fileDao := NewFileDao(nil, baseData, lockDao)
	uploadDao := NewUploadDao(fileDao, lockDao)

	const (
		repoType = "models"
		orgRepo  = "dingo-local/demo"
		revision = "main"
		commit   = "abc123"
	)
	err := uploadDao.writeMeta(repoType, orgRepo, revision, commit, []LocalManifestFile{{
		Path:   "config.json",
		Sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Size:   12,
	}})
	if err != nil {
		t.Fatalf("writeMeta failed: %v", err)
	}

	metaPath := fmt.Sprintf("%s/api/%s/%s/revision/%s/meta_get.json", config.SysConfig.Repos(), repoType, orgRepo, revision)
	cacheContent, err := fileDao.ReadCacheRequest(metaPath)
	if err != nil {
		t.Fatalf("ReadCacheRequest failed: %v", err)
	}
	var body map[string]interface{}
	if err = sonic.Unmarshal(cacheContent.OriginContent, &body); err != nil {
		t.Fatalf("unmarshal metadata failed: %v", err)
	}
	if body["id"] != orgRepo {
		t.Fatalf("expected id %q, got %#v", orgRepo, body["id"])
	}
}

func TestEnsureLocalMetadataIDBackfillsOldMetadata(t *testing.T) {
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{Upload: config.Upload{Namespace: "dingo-local"}}
	defer func() { config.SysConfig = oldConfig }()

	metaDao := &MetaDao{}
	body := []byte(`{"sha":"abc123","siblings":[{"rfilename":"config.json"}],"usedStorage":12}`)
	cacheContent := &common.CacheContent{
		Headers:       map[string]string{"content-length": fmt.Sprintf("%d", len(body))},
		OriginContent: body,
	}
	cacheContent, err := metaDao.ensureLocalMetadataID("dingo-local/demo", cacheContent)
	if err != nil {
		t.Fatalf("ensureLocalMetadataID failed: %v", err)
	}

	var metadata map[string]interface{}
	if err = sonic.Unmarshal(cacheContent.OriginContent, &metadata); err != nil {
		t.Fatalf("unmarshal normalized metadata failed: %v", err)
	}
	if metadata["id"] != "dingo-local/demo" {
		t.Fatalf("expected id to be backfilled, got %#v", metadata["id"])
	}
	if cacheContent.Headers["content-length"] != fmt.Sprintf("%d", len(cacheContent.OriginContent)) {
		t.Fatalf("content-length was not updated")
	}
}
