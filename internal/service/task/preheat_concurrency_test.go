// Copyright 2026 DataCanvas Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/model/query"
	"dingospeed/pkg/config"
)

// preheatFixture stands up a task whose files all still need transferring: no
// blobs exist on disk, so GetFileOffset reports 0 against a non-zero size.
func preheatFixture(t *testing.T, files int) *PreheatCacheTask {
	t.Helper()
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/paths-info/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		fmt.Fprintf(w, `[{"type":"file","path":%q,"oid":"oid-%s","size":64}]`, name, name)
	}))
	t.Cleanup(upstream.Close)

	config.SysConfig = &config.Config{
		Server:   config.ServerConfig{Online: true, Repos: t.TempDir(), HfScheme: "http", HfNetLoc: strings.TrimPrefix(upstream.URL, "http://")},
		Retry:    config.Retry{Attempts: 1},
		Download: config.Download{BlockSize: 4},
	}
	base := data.NewBaseData()

	siblings := make([]string, 0, files)
	for i := 0; i < files; i++ {
		siblings = append(siblings, fmt.Sprintf(`{"rfilename":"shard-%02d.bin"}`, i))
	}
	var meta dao.CommitHfSha
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"sha":"commit1","siblings":[%s]}`, strings.Join(siblings, ","))), &meta); err != nil {
		t.Fatal(err)
	}
	return &PreheatCacheTask{
		CacheTask: CacheTask{Ctx: context.Background(), Job: &query.CreateCacheJobReq{Namespace: "huggingface", Repo: "owner/model", Datatype: "models"}},
		Sha:       &meta,
		FileDao:   dao.NewFileDao(nil, base, dao.NewLockDao(base)),
	}
}

// TestPreheatTransfersFilesConcurrently pins the behaviour the semaphore was
// always meant to provide.
//
// The limiter used to guard a *synchronous* call - acquire, run the whole
// transfer, release - so only one file was ever in flight no matter how large
// the bound was. Combined with download.remoteFileRangeSize defaulting to 0
// (no range splitting inside a file), a preheat job opened exactly one upstream
// connection. hf-mirror throttles a single connection to a few MB/s, which is
// what made a 52GB model quote nearly three hours on hd-04.
func TestPreheatTransfersFilesConcurrently(t *testing.T) {
	p := preheatFixture(t, preheatFileConcurrency*2)

	var mu sync.Mutex
	var inFlight, peak int
	var calls atomic.Int32

	p.transfer = func(_, _, _, _, _, _ string, _, _ int64) error {
		calls.Add(1)
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		// Hold the slot long enough that genuinely parallel transfers overlap.
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}

	if err := p.preheatProcess("huggingface/owner/model"); err != nil {
		t.Fatalf("preheatProcess: %v", err)
	}

	mu.Lock()
	got := peak
	mu.Unlock()
	t.Logf("%d files, peak concurrent transfers: %d (bound %d)", calls.Load(), got, preheatFileConcurrency)

	if calls.Load() != int32(preheatFileConcurrency*2) {
		t.Fatalf("every file must be transferred: got %d of %d", calls.Load(), preheatFileConcurrency*2)
	}
	if got < 2 {
		t.Fatalf("transfers ran one at a time (peak %d); the concurrency bound is not in effect", got)
	}
	if got > preheatFileConcurrency {
		t.Fatalf("peak %d exceeds the bound of %d", got, preheatFileConcurrency)
	}
}

// TestPreheatReportsTransferFailure keeps a failing file from being swallowed
// now that transfers no longer run inline.
func TestPreheatReportsTransferFailure(t *testing.T) {
	p := preheatFixture(t, 6)
	want := errors.New("upstream refused")
	var once sync.Once
	p.transfer = func(_, _, name, _, _, _ string, _, _ int64) error {
		var err error
		once.Do(func() { err = want })
		if err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
		return nil
	}
	if err := p.preheatProcess("huggingface/owner/model"); !errors.Is(err, want) {
		t.Fatalf("expected the transfer error to surface, got %v", err)
	}
}

// TestPreheatStopsOnCancel makes sure a cancelled job does not keep queueing
// work and returns the cancellation rather than reporting success.
func TestPreheatStopsOnCancel(t *testing.T) {
	p := preheatFixture(t, preheatFileConcurrency*3)
	ctx, cancel := context.WithCancel(context.Background())
	p.Ctx = ctx
	var started atomic.Int32
	p.transfer = func(_, _, _, _, _, _ string, _, _ int64) error {
		if started.Add(1) == 2 {
			cancel()
		}
		time.Sleep(10 * time.Millisecond)
		return nil
	}
	err := p.preheatProcess("huggingface/owner/model")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if int(started.Load()) == preheatFileConcurrency*3 {
		t.Fatal("cancellation did not stop the job queueing further files")
	}
}
