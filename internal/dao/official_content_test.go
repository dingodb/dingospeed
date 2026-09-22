package dao

import (
	"os"
	"testing"
)

func TestOfficialContentRejectsSameSizeCorruption(t *testing.T) {
	u, _ := newTestUploadDao(t)
	body := []byte("official payload")
	mustUpload(t, u, uploadParam("file.bin", body), body)
	files := []LocalManifestFile{manifestItem("file.bin", body)}
	if err := VerifyPublishedSHA256("models", "dingo-local/demo", files); err != nil {
		t.Fatal(err)
	}
	path := localBlobPath("models", "dingo-local/demo", files[0].Sha256)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = VerifyPublishedSHA256("models", "dingo-local/demo", files); err == nil {
		t.Fatal("same-size changed payload accepted")
	}
}
