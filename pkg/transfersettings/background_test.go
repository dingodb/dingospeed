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

package transfersettings

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dingospeed/pkg/config"
)

// TestBackgroundLeavesForegroundSlots pins the reservation that lets a preheat
// job raise its concurrency without starving interactive downloads.
//
// On hd-04 a preheat held every HuggingFace transfer slot and a user's
// download of a not-yet-cached file waited 150s behind it.
func TestBackgroundLeavesForegroundSlots(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir()}}

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	var open []*http.Response
	t.Cleanup(func() {
		close(release)
		for _, r := range open {
			_ = r.Body.Close()
		}
	})

	limit := Current().HuggingFace
	bgLimit := backgroundLimitFor(kindIndex("huggingface"))()
	if bgLimit >= limit {
		t.Fatalf("background limit %d leaves nothing of %d for foreground", bgLimit, limit)
	}
	get := func(ctx context.Context) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		return Do("huggingface", server.Client(), req)
	}

	for i := 0; i < bgLimit; i++ {
		resp, err := get(WithBackground(context.Background()))
		if err != nil {
			t.Fatalf("background transfer %d of %d: %v", i+1, bgLimit, err)
		}
		open = append(open, resp)
	}

	// Control: background is capped, so one more must wait.
	ctx, cancel := context.WithTimeout(WithBackground(context.Background()), 300*time.Millisecond)
	defer cancel()
	if resp, err := get(ctx); err == nil {
		open = append(open, resp)
		t.Fatalf("background exceeded its limit of %d", bgLimit)
	}

	for i := 0; i < limit-bgLimit; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		start := time.Now()
		resp, err := get(ctx)
		cancel()
		if err != nil {
			t.Fatalf("foreground transfer starved with background at its cap: %v", err)
		}
		open = append(open, resp)
		t.Logf("foreground %d/%d admitted in %v with %d background transfers open", i+1, limit-bgLimit, time.Since(start), bgLimit)
	}
}
