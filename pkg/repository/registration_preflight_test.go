package repository

import (
	"os"
	"testing"
)

func TestRegistrationPreflightDoesNotCreateOrReserve(t *testing.T) {
	root := t.TempDir()
	k := RepoKey{Namespace: ModelScope, RepoType: "models", Repo: "owner/model"}
	d := Remote(k)
	if err := CheckRegistration(root, d); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root, k); !os.IsNotExist(err) {
		t.Fatalf("preflight wrote marker: %v", err)
	}
	conflict := Remote(RepoKey{Namespace: ModelScope, RepoType: "models", Repo: "Owner/model"})
	if err := Register(root, conflict); err != nil {
		t.Fatal(err)
	}
	if err := Register(root, d); err == nil {
		t.Fatal("registration failed to recheck a concurrent conflict")
	}
	if err := CheckRegistration(root, d); err == nil {
		t.Fatal("preflight missed conflict")
	}
}
