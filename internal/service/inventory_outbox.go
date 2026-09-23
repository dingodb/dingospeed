package service

import (
	"bytes"
	"context"
	"dingospeed/internal/dao"
	"dingospeed/pkg/config"
	"dingospeed/pkg/inventory"
	"dingospeed/pkg/repository"
	"encoding/json"
	"errors"
	"fmt"
	"go.uber.org/zap"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxBaselineBuildAttempts = 3

// Only local baseline construction consumes the scan budget. Network retries
// reuse a fixed report and must not exhaust it.
type baselineBuildError struct{ error }

var inventoryConnected = make(chan struct{}, 1)

func wakeInventoryConnection() {
	select {
	case inventoryConnected <- struct{}{}:
	default:
	}
}

func inventoryPOST(ctx context.Context, path string, in, out any) error {
	base := strings.TrimRight(config.SysConfig.Registration().HTTPURL, "/")
	if base == "" {
		return fmt.Errorf("Scheduler HTTP URL is not configured")
	}
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("inventory HTTP %d: %s", resp.StatusCode, b)
	}
	d := json.NewDecoder(io.LimitReader(resp.Body, 65536))
	if err = d.Decode(out); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("invalid acknowledgement")
	}
	return nil
}
func inventoryBackoff(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	return time.Second*time.Duration(1<<attempt) + time.Duration(rand.Intn(1000))*time.Millisecond
}

// The timer schedules pending retries only. Empty established queues block on
// events and never enumerate repositories or transmit inventory.
func (s *SchedulerService) ReconcilePublications() {
	needSession := true
	delay := time.Duration(0)
	attempt := 0
	for {
		timer := time.NewTimer(delay)
		select {
		case <-s.Ctx.Done():
			timer.Stop()
			return
		case <-inventoryConnected:
			needSession = true
		case <-inventory.Wake:
		case <-dao.PublishedChanges: // legacy wake-up is harmless; facts come from the journal
		case <-timer.C:
		}
		timer.Stop()
		if !config.SysConfig.Registration().Enabled {
			delay = 24 * time.Hour
			continue
		}
		if needSession {
			if err := s.openInventorySession(s.Ctx); err != nil {
				attempt++
				delay = inventoryBackoff(attempt)
				zap.S().Warnf("inventory session pending: %v", err)
				continue
			}
			needSession = false
			attempt = 0
		}
		if err := s.SyncPublications(s.Ctx); err != nil {
			zap.S().Warnf("inventory reporting pending: %v", err)
		}
		delay = nextInventoryWake()
	}
}
func nextInventoryWake() time.Duration {
	s, err := inventory.Read(config.SysConfig.Repos())
	if err != nil {
		return time.Minute
	}
	next := time.Now().Add(24 * time.Hour)
	if s.ReconcileEpoch != "" {
		if !s.BuildBlocked {
			next = s.RetryAt
		}
	} else {
		for _, e := range s.Entries {
			if e.Due.Before(next) {
				next = e.Due
			}
		}
	}
	d := time.Until(next)
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}
func (s *SchedulerService) openInventorySession(ctx context.Context) error {
	r := config.SysConfig.Registration()
	var remote inventory.Session
	if err := inventoryPOST(ctx, "/api/v1/upload-inventory/nodes/"+url.PathEscape(r.NodeID)+"/session", struct{}{}, &remote); err != nil {
		return err
	}
	if remote.InstanceID != r.NodeID {
		return fmt.Errorf("inventory session node mismatch")
	}
	root := config.SysConfig.Repos()
	return inventory.Update(root, func(q *inventory.State) error {
		if q.NodeID != "" && q.NodeID != r.NodeID {
			return fmt.Errorf("inventory node identity changed")
		}
		q.NodeID = r.NodeID
		if remote.PendingEpoch != "" {
			if q.ReconcileEpoch != remote.PendingEpoch {
				q.ReconcileEpoch = remote.PendingEpoch
				q.Baseline = nil
				q.BuildAttempts = 0
				q.BuildBlocked = false
				q.RetryAt = time.Time{}
				q.Attempts = 0
			}
		} else if q.Baseline != nil && q.Baseline.Epoch == remote.Epoch {
			// A baseline commit response may have been lost; resend the same bytes.
		} else if q.Epoch != remote.Epoch {
			return fmt.Errorf("inventory state lost or Scheduler reset; explicitly reconcile this node")
		}
		return nil
	})
}

func (s *SchedulerService) SyncPublications(ctx context.Context) error {
	root := config.SysConfig.Repos()
	q, err := inventory.Read(root)
	if err != nil {
		return err
	}
	if q.ReconcileEpoch != "" {
		if q.BuildBlocked {
			s.reportReconcileProgress(ctx, q.ReconcileEpoch, "needs_attention", q.Error)
			return nil
		}
		if time.Now().Before(q.RetryAt) {
			return nil
		}
		err = s.sendBaseline(ctx)
		if err != nil {
			status := "retrying"
			var buildErr baselineBuildError
			updateErr := inventory.Update(root, func(current *inventory.State) error {
				if current.ReconcileEpoch != q.ReconcileEpoch {
					return nil
				}
				current.Attempts++
				current.RetryAt = time.Now().Add(inventoryBackoff(current.Attempts))
				current.Error = err.Error()
				if errors.As(err, &buildErr) {
					current.BuildAttempts++
					if current.BuildAttempts >= maxBaselineBuildAttempts {
						current.BuildBlocked = true
						current.Error = "baseline construction failed three times; repair the cause and explicitly reconcile again: " + err.Error()
						status = "needs_attention"
					}
				}
				return nil
			})
			if updateErr != nil {
				return fmt.Errorf("persist baseline retry state: %w (build error: %v)", updateErr, err)
			}
			current, readErr := inventory.Read(root)
			if readErr == nil && current.ReconcileEpoch == q.ReconcileEpoch {
				s.reportReconcileProgress(ctx, q.ReconcileEpoch, status, current.Error)
			}
		}
		return err
	}
	if q.Epoch == "" {
		return fmt.Errorf("inventory baseline not initialized")
	}
	for id, e := range q.Entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().Before(e.Due) {
			continue
		}
		err = s.sendRepository(ctx, e.Key)
		if err != nil {
			_ = inventory.Update(root, func(q *inventory.State) error {
				if current := q.Entries[id]; current != nil {
					current.Attempts++
					current.Due = time.Now().Add(inventoryBackoff(current.Attempts))
					current.Error = err.Error()
				}
				return nil
			})
		}
	}
	return nil
}
func repoKey(k inventory.Key) repository.RepoKey {
	return repository.RepoKey{Namespace: k.Namespace, RepoType: k.RepoType, Repo: k.Repo}
}
func (s *SchedulerService) recoverInventory(k inventory.Key) error {
	return dao.NewUploadDao(s.metaService.fileDao, nil).RecoverInventory(repoKey(k))
}

func (s *SchedulerService) sendRepository(ctx context.Context, k inventory.Key) error {
	if err := s.recoverInventory(k); err != nil {
		return err
	}
	root := config.SysConfig.Repos()
	p, err := func() (*inventory.Report, error) {
		inventory.Barrier.Lock()
		defer inventory.Barrier.Unlock()
		q, err := inventory.Read(root)
		if err != nil {
			return nil, err
		}
		e := q.Entries[k.ID()]
		if e == nil {
			return nil, nil
		}
		if q.ReconcileEpoch != "" {
			return nil, nil
		}
		if e.Operation != nil {
			return nil, fmt.Errorf("repository operation incomplete")
		}
		if e.Report != nil {
			return e.Report, nil
		}
		p := &inventory.Report{Version: 2, InstanceID: q.NodeID, Epoch: q.Epoch, Sequence: e.Sequence, Key: k, Deleted: e.Deleted, Files: []inventory.File{}}
		if !e.Deleted {
			d, err := repository.Read(root, repoKey(k))
			if err != nil {
				return nil, err
			}
			files, err := s.scanInventory([]repository.Descriptor{d})
			if err != nil {
				return nil, err
			}
			p.Files = files
		}
		err = inventory.Update(root, func(q *inventory.State) error { q.Entries[k.ID()].Report = p; return nil })
		return p, err
	}()
	if err != nil || p == nil {
		return err
	}
	var a inventory.Ack
	if err = inventoryPOST(ctx, "/api/v1/upload-inventory/reports", p, &a); err != nil {
		return err
	}
	return inventory.Confirm(root, p, a)
}
func (s *SchedulerService) scanInventory(descriptors []repository.Descriptor) ([]inventory.File, error) {
	snap, err := s.scanUploadDescriptors(&UploadInventorySnapshot{Complete: true, Items: []UploadInventoryItem{}}, descriptors)
	if err != nil {
		return nil, err
	}
	if !snap.Complete {
		return nil, fmt.Errorf("incomplete inventory: %s", snap.Error)
	}
	files := make([]inventory.File, 0, len(snap.Items))
	for _, f := range snap.Items {
		files = append(files, inventory.File{Key: inventory.Key{Namespace: f.Namespace, RepoType: f.RepoType, Repo: f.Repo}, Path: f.Path, SHA256: f.SHA256, Size: f.Size})
	}
	return files, nil
}
func (s *SchedulerService) sendBaseline(ctx context.Context) (resultErr error) {
	building := false
	defer func() {
		if building && resultErr != nil {
			resultErr = baselineBuildError{resultErr}
		}
	}()
	root := config.SysConfig.Repos()
	q, err := inventory.Read(root)
	if err != nil {
		return err
	}
	if q.BuildBlocked {
		return fmt.Errorf("baseline requires explicit reconciliation")
	}
	if q.Baseline == nil {
		// Authenticate the requested generation before doing expensive disk work.
		if err = s.openInventorySession(ctx); err != nil {
			return err
		}
		building = true
		s.reportReconcileProgress(ctx, q.ReconcileEpoch, "scanning", "")
		q, err = inventory.Read(root)
		if err != nil {
			return err
		}
		for _, e := range q.Entries {
			if e.Operation != nil {
				if err = s.recoverInventory(e.Key); err != nil {
					return err
				}
			}
		}
		err = func() error {
			inventory.Barrier.Lock()
			defer inventory.Barrier.Unlock()
			q, err = inventory.Read(root)
			if err != nil {
				return err
			}
			for _, e := range q.Entries {
				if e.Operation != nil {
					return fmt.Errorf("local operation recovery pending")
				}
			}
			descriptors, err := repository.ListHosted(root)
			if err != nil {
				return err
			}
			files, err := s.scanInventory(descriptors)
			if err != nil {
				return err
			}
			// Inventory trusts Speed's published metadata. scanInventory checks
			// manifests, blob presence, sizes and completion markers; rebuilding
			// the baseline must not read payloads to recompute their SHA256.
			p := &inventory.Report{Version: 2, InstanceID: q.NodeID, Epoch: q.ReconcileEpoch, Baseline: true, Files: files}
			return inventory.Update(root, func(st *inventory.State) error {
				if st.ReconcileEpoch != q.ReconcileEpoch {
					return fmt.Errorf("reconciliation changed")
				}
				st.Cutoff = st.Sequence
				st.Baseline = p
				return nil
			})
		}()
		if err != nil {
			return err
		}
	}
	building = false
	q, err = inventory.Read(root)
	if err != nil {
		return err
	}
	if q.Baseline == nil {
		return fmt.Errorf("baseline unavailable")
	}
	var a inventory.Ack
	if err = inventoryPOST(ctx, "/api/v1/upload-inventory/reports", q.Baseline, &a); err != nil {
		return err
	}
	return inventory.Confirm(root, q.Baseline, a)
}

func (s *SchedulerService) reportReconcileProgress(ctx context.Context, epoch, status, message string) {
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var result map[string]bool
	_ = inventoryPOST(call, "/api/v1/upload-inventory/nodes/"+url.PathEscape(config.SysConfig.Registration().NodeID)+"/reconcile-progress", map[string]string{"epoch": epoch, "status": status, "error": message}, &result)
}
