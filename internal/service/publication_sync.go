package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/pkg/config"
	pb "dingospeed/pkg/proto/manager"
	"dingospeed/pkg/repository"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type UploadInventoryItem struct {
	Namespace string `json:"namespace"`
	RepoType  string `json:"repoType"`
	Repo      string `json:"repo"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

type UploadInventorySnapshot struct {
	Version        int                   `json:"version"`
	InstanceID     string                `json:"instanceId"`
	Epoch          string                `json:"epoch"`
	EpochStartedAt time.Time             `json:"epochStartedAt"`
	Sequence       uint64                `json:"sequence"`
	GeneratedAt    time.Time             `json:"generatedAt"`
	Complete       bool                  `json:"complete"`
	Error          string                `json:"error,omitempty"`
	Items          []UploadInventoryItem `json:"items"`
	ReportToken    string                `json:"reportToken,omitempty"`
}

var inventorySnapshotMu sync.Mutex

func uploadInventoryPath() string {
	return filepath.Join(config.SysConfig.Repos(), ".upload-inventory", "snapshot.json")
}

func LoadUploadInventorySnapshot() (*UploadInventorySnapshot, error) {
	inventorySnapshotMu.Lock()
	defer inventorySnapshotMu.Unlock()
	b, err := os.ReadFile(uploadInventoryPath())
	if err != nil {
		return nil, err
	}
	var snap UploadInventorySnapshot
	if err = json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// ReconcilePublications is now a node-wide uploaded inventory reconciler. Each
// local mutation is only a wake-up; the durable full scan is the source of
// truth, so a crash between the mutation and notification is repaired on start.
func (s *SchedulerService) ReconcilePublications() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if config.SysConfig.IsCluster() {
			if err := s.SyncPublications(s.Ctx); err != nil {
				zap.S().Warnf("upload inventory sync pending: %v", err)
			}
		}
		select {
		case <-s.Ctx.Done():
			return
		case <-ticker.C:
		case <-dao.PublishedChanges:
		}
	}
}

func (s *SchedulerService) SyncPublications(ctx context.Context) error {
	snap, err := s.buildUploadInventory()
	if err != nil {
		return err
	}
	if err = persistUploadInventory(snap); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	_, err = s.Client.IngestRepository(callCtx, &pb.IngestRepositoryRequest{InstanceId: snap.InstanceID, Revision: "inventory", Commit: snap.ReportToken, Online: config.SysConfig.Online()})
	return err
}

func (s *SchedulerService) buildUploadInventory() (*UploadInventorySnapshot, error) {
	previous, loadErr := LoadUploadInventorySnapshot()
	now := time.Now().UTC()
	snap := &UploadInventorySnapshot{Version: 1, InstanceID: config.SysConfig.Registration().NodeID, GeneratedAt: now, Complete: true, Items: make([]UploadInventoryItem, 0)}
	snap.ReportToken = uuid.NewString()
	if loadErr == nil && previous.Epoch != "" && previous.InstanceID == snap.InstanceID {
		snap.Epoch, snap.EpochStartedAt, snap.Sequence = previous.Epoch, previous.EpochStartedAt, previous.Sequence+1
	} else {
		snap.Epoch, snap.EpochStartedAt, snap.Sequence = uuid.NewString(), now, 1
	}
	if snap.InstanceID == "" {
		return nil, fmt.Errorf("scheduler discovery instanceId is empty")
	}
	descriptors, err := repository.List(config.SysConfig.Repos())
	if err != nil {
		snap.Complete = false
		snap.Error = err.Error()
		return snap, nil
	}
	seen := make(map[string]struct{})
	errorsFound := make([]string, 0)
	for _, d := range descriptors {
		if d.Source != "hosted" {
			continue
		}
		entries, readErr := os.ReadDir(filepath.Join(d.APIRoot(config.SysConfig.Repos()), "revision"))
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			errorsFound = append(errorsFound, d.ID()+": "+readErr.Error())
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			local, readErr := s.metaService.GetLocalSnapshot(d.RepoType, d.ID(), entry.Name())
			if readErr != nil {
				errorsFound = append(errorsFound, d.ID()+"/"+entry.Name()+": "+readErr.Error())
				continue
			}
			if entry.Name() == local.Commit {
				continue
			} // immutable snapshot directory, not a live local revision
			manifest := make([]dao.LocalManifestFile, len(local.Files))
			for i, file := range local.Files {
				manifest[i] = dao.LocalManifestFile{Path: file.Path, Sha256: file.Sha256, Size: file.Size}
			}
			if verifyErr := dao.VerifyPublishedFiles(d.RepoType, d.ID(), local.Commit, manifest); verifyErr != nil {
				errorsFound = append(errorsFound, d.ID()+"/"+entry.Name()+": "+verifyErr.Error())
				continue
			}
			for _, file := range local.Files {
				key := strings.Join([]string{d.Namespace, d.RepoType, d.Repo, file.Path, file.Sha256}, "\x00")
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				snap.Items = append(snap.Items, UploadInventoryItem{Namespace: d.Namespace, RepoType: d.RepoType, Repo: d.Repo, Path: file.Path, SHA256: file.Sha256, Size: file.Size})
			}
		}
	}
	sort.Slice(snap.Items, func(i, j int) bool {
		a, b := snap.Items[i], snap.Items[j]
		return strings.Join([]string{a.Namespace, a.RepoType, a.Repo, a.Path, a.SHA256}, "\x00") < strings.Join([]string{b.Namespace, b.RepoType, b.Repo, b.Path, b.SHA256}, "\x00")
	})
	if len(errorsFound) > 0 {
		snap.Complete = false
		snap.Error = strings.Join(errorsFound, "; ")
	}
	return snap, nil
}

func persistUploadInventory(snap *UploadInventorySnapshot) error {
	inventorySnapshotMu.Lock()
	defer inventorySnapshotMu.Unlock()
	dest := uploadInventoryPath()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".snapshot-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, dest)
}
