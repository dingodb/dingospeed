package repository

import (
	"fmt"
	"strings"
)

// FromStorage decodes unchanged remote identities and explicitly encoded hosted ones.
func FromStorage(repoType, org, repo string) (RepoKey, error) {
	k := RepoKey{Namespace: "huggingface", RepoType: repoType, Repo: repo}
	if strings.HasPrefix(org, "dingo-local/") {
		k.Namespace = strings.TrimPrefix(org, "dingo-local/")
	} else if strings.HasPrefix(org, "modelscope/") {
		k.Namespace = "modelscope"
		k.Repo = strings.TrimPrefix(org, "modelscope/") + "/" + repo
	} else if org != "" {
		k.Repo = org + "/" + repo
	}
	return k, k.Validate()
}

// Storage leaves HF owner/repo exactly as old SQL and old peers expect.
func (k RepoKey) Storage() (org, repo string, err error) {
	if err = k.Validate(); err != nil {
		return
	}
	repo = k.Repo
	switch k.Namespace {
	case "huggingface":
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) == 2 {
			org, repo = parts[0], parts[1]
		}
	case "modelscope":
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("ModelScope needs owner/repo")
		}
		org, repo = "modelscope/"+parts[0], parts[1]
	default:
		org = "dingo-local/" + k.Namespace
	}
	if k.Namespace != "huggingface" && (len(org) > 100 || len(repo) > 100 || len(org)+1+len(repo) > 100) {
		err = fmt.Errorf("repository identity exceeds unchanged SQL VARCHAR(100) capacity")
	}
	return
}
