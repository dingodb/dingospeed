package config

import (
	"bytes"
	"testing"
)

func TestMarshalConfigForLogRedactsUploadTokenWithoutMutatingConfig(t *testing.T) {
	const secret = "upload-secret-must-not-leak"
	cfg := Config{Upload: Upload{Token: secret, Host: "127.0.0.1", Port: 8091}}

	logged, err := marshalConfigForLog(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(logged, []byte(secret)) {
		t.Fatalf("serialized log contains upload token: %s", logged)
	}
	if !bytes.Contains(logged, []byte("token: <redacted>")) {
		t.Fatalf("serialized log does not contain redaction marker: %s", logged)
	}
	if cfg.Upload.Token != secret {
		t.Fatalf("runtime upload token was mutated: %q", cfg.Upload.Token)
	}
}
