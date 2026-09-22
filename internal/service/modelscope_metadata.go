package service

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
)

// LatestCommitter describes the complete requested tree. A Files[i].Revision
// describes only that file's last change and must never substitute for it.
func (m *ModelscopeService) cacheRepositoryMetadata(k repository.RepoKey, revision string, envelope *modelscopeEnvelope) (map[string]interface{}, error) {
	commit := envelope.Data.LatestCommitter.Id
	if commit == "" {
		commit = envelope.Data.LatestCommitter.ShortId
	}
	if repository.Segment(revision) != nil || repository.Segment(commit) != nil {
		return nil, fmt.Errorf("ModelScope response has no repository revision")
	}
	siblings := []map[string]interface{}{}
	infos := []*common.PathsInfo{}
	var size int64
	for _, file := range envelope.Data.Files {
		if file.Type == "tree" || file.Type == "directory" {
			continue
		}
		name := file.Path
		if name == "" {
			name = file.Name
		}
		oid := file.Sha256
		if oid == "" {
			oid = file.Sha1
		}
		if repository.Relative(name) != nil || repository.Segment(oid) != nil || file.Size < 0 {
			return nil, fmt.Errorf("invalid ModelScope file metadata")
		}
		siblings = append(siblings, map[string]interface{}{"rfilename": name, "size": file.Size, "blobId": oid, "fileRevision": file.Revision})
		infos = append(infos, &common.PathsInfo{Type: "file", Path: name, Oid: oid, Size: file.Size})
		size += file.Size
	}
	meta := map[string]interface{}{"namespace": k.Namespace, "repoType": k.RepoType, "repo": k.Repo, "id": k.ID(), "sha": commit, "siblings": siblings, "usedStorage": size}
	body, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	// Publish the revision pointer only after its complete tree is available.
	for _, info := range infos {
		if err = m.fileDao.CacheProviderFileInfo(k, commit, revision, info); err != nil {
			return nil, err
		}
	}
	for _, ref := range []string{commit, revision} {
		target := filepath.Join(k.Revision(config.SysConfig.Repos(), ref), "meta_get.json")
		if err = m.fileDao.CacheProviderResponse(target, commit, revision, body); err != nil {
			return nil, err
		}
	}
	return meta, nil
}
