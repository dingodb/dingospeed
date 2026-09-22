package dao

import (
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"fmt"
)

func RegisterHosted(repoType, namespace, repo string) error {
	k := repository.RepoKey{Namespace: namespace, RepoType: repoType, Repo: repo}
	if repoType == "spaces" {
		return fmt.Errorf("spaces uploads are not supported")
	}
	if _, _, err := k.Storage(); err != nil {
		return err
	}
	return repository.Register(config.SysConfig.Repos(), repository.Hosted(k))
}

func RepositoryKey(repoType, id string) repository.RepoKey {
	k, _ := repository.ParseID(repoType, id)
	return k
}

func UpstreamRepo(repoType, id string) (string, error) {
	k, err := repository.ParseID(repoType, id)
	if err != nil {
		return "", err
	}
	// Provider namespaces are reserved server configuration, so the upstream
	// identity remains known even when the cache filesystem is unavailable.
	if k.Namespace != repository.HuggingFace {
		return "", fmt.Errorf("repository has no Hugging Face upstream")
	}
	return k.Repo, nil
}
