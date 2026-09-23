// Package inventory owns only uploaded inventory, never remote cache progress.
package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type Key struct {
	Namespace string `json:"namespace"`
	RepoType  string `json:"repoType"`
	Repo      string `json:"repo"`
}

func (k Key) ID() string { return k.Namespace + "\x00" + k.RepoType + "\x00" + k.Repo }

type File struct {
	Key
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Report struct {
	Version    int    `json:"version"`
	InstanceID string `json:"instanceId"`
	Epoch      string `json:"epoch"`
	Sequence   uint64 `json:"sequence"`
	Baseline   bool   `json:"baseline"`
	Key
	Deleted bool   `json:"deleted"`
	Files   []File `json:"files"`
}

func (r Report) Digest() string {
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Ack struct {
	Epoch    string `json:"epoch"`
	Sequence uint64 `json:"sequence"`
	Digest   string `json:"digest"`
	Status   string `json:"status"`
}
type Session struct {
	InstanceID   string `json:"instanceId"`
	Epoch        string `json:"epoch"`
	PendingEpoch string `json:"pendingEpoch"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}
