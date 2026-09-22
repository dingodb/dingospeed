package dao

import "fmt"

// VerifyPublishedSHA256 is the deliberately expensive official-management check.
// Completion bits alone cannot prove the content still matches its identity.
// The existing lightweight inventory/publication checks keep their behavior.
func VerifyPublishedSHA256(repoType, id string, files []LocalManifestFile) error {
	for _, f := range files {
		digest, err := hashDingCachePayload(localBlobPath(repoType, id, f.Sha256), f.Size)
		if err != nil {
			return fmt.Errorf("selected content %s is unreadable: %w", f.Path, err)
		}
		if digest != f.Sha256 {
			return fmt.Errorf("selected content %s SHA256 changed", f.Path)
		}
	}
	return nil
}
