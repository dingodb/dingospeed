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

package util

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/transfersettings"
)

// TestMetadataIsNotStarvedByContentStreams reproduces the hd-04 stall seen when
// a preheat job transferred several files at once.
//
// Every HuggingFace request used to pass through one gate. Content streams hold
// their slot until the range body is read, so once concurrent transfers filled
// every slot, the metadata lookup behind an ordinary cached read waited behind
// them and the request timed out. Metadata now has a gate of its own.
func TestMetadataIsNotStarvedByContentStreams(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stream":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("chunk"))
			w.(http.Flusher).Flush()
			select { // a range still being transferred
			case <-release:
			case <-r.Context().Done():
			}
		case "/meta":
			_, _ = io.WriteString(w, `{"sha":"main"}`)
		}
	}))
	t.Cleanup(server.Close)
	config.SysConfig = &config.Config{
		Server: config.ServerConfig{Repos: t.TempDir(), HfScheme: "http", HfNetLoc: strings.TrimPrefix(server.URL, "http://")},
		Retry:  config.Retry{Attempts: 1},
	}

	slots := transfersettings.Current().HuggingFace
	var entered, done sync.WaitGroup
	entered.Add(slots)
	done.Add(slots)
	for i := 0; i < slots; i++ {
		go func() {
			defer done.Done()
			once := sync.Once{}
			_ = GetStreamContext(context.Background(), server.URL, "/stream", map[string]string{}, func(resp *http.Response) error {
				once.Do(entered.Done)
				<-release // keep the slot, as an unread range body does
				return nil
			})
		}()
	}
	waitOrFail(t, &entered, 5*time.Second, "content streams never filled the transfer gate")
	t.Cleanup(func() { close(release); done.Wait() })

	// Control: the transfer gate really is full, so this test cannot pass by
	// accident.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := GetStreamContext(ctx, server.URL, "/stream", map[string]string{}, func(*http.Response) error { return nil }); err == nil {
		t.Fatal("an extra content stream got through a gate that should be full")
	}

	start := time.Now()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	resp, err := GetContext(ctx2, "/meta", map[string]string{})
	if err != nil {
		t.Fatalf("metadata request starved behind %d content streams: %v", slots, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metadata status %d", resp.StatusCode)
	}
	t.Logf("%d content streams holding the transfer gate; metadata answered in %v", slots, time.Since(start))
}

func waitOrFail(t *testing.T, wg *sync.WaitGroup, d time.Duration, msg string) {
	t.Helper()
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatal(msg)
	}
}
