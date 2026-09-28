package service

import (
	"fmt"
	"strings"

	"dingospeed/pkg/config"
)

// @ai is reserved outside ModelScope's owner alphabet. Keeping it in the
// platform repository key isolates tasks, manifests, locks and cached files.
// Only the upstream request strips the prefix.
func modelscopeOrigin(repo string) (base, upstream string, err error) {
	parts := strings.Split(repo, "/")
	if len(parts) == 3 && parts[0] == "@ai" {
		return "https://www.modelscope.ai", strings.Join(parts[1:], "/"), nil
	}
	if len(parts) != 2 || strings.HasPrefix(parts[0], "@") {
		return "", "", fmt.Errorf("invalid ModelScope repository identity")
	}
	return strings.TrimRight(config.SysConfig.Modelscope.OfficialBaseURL, "/"), repo, nil
}
