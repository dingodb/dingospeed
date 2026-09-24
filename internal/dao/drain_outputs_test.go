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

package dao

import (
	"context"
	"testing"
	"time"

	"dingospeed/pkg/common"
)

// fakeRange stands in for one range task. OutResult waits for hold (when set)
// before emitting, modelling an earlier range whose transfer is still running.
type fakeRange struct {
	resp chan []byte
	out  string
	hold chan struct{}
}

func (f *fakeRange) GetTaskNo() int                   { return 0 }
func (f *fakeRange) DoTask()                          {}
func (f *fakeRange) GetCancelFun() context.CancelFunc { return func() {} }
func (f *fakeRange) SetTaskSize(int)                  {}
func (f *fakeRange) GetResponseChan() chan []byte     { return f.resp }
func (f *fakeRange) OutResult() {
	if f.hold != nil {
		<-f.hold
	}
	f.resp <- []byte(f.out)
}

func nextChunk(t *testing.T, ch chan []byte, within time.Duration) (string, bool) {
	t.Helper()
	select {
	case b := <-ch:
		return string(b), true
	case <-time.After(within):
		return "", false
	}
}

// TestDrainOutputsUnorderedDoesNotWaitForEarlierRanges is why splitting a file
// into ranges only paid off for client downloads: drained in order, a later
// range blocks behind the earlier ones once its buffer fills, so a preheat that
// ignores order still moved about one stream per file.
func TestDrainOutputsUnorderedDoesNotWaitForEarlierRanges(t *testing.T) {
	resp := make(chan []byte, 8)
	hold := make(chan struct{})
	tasks := []common.DownloadTask{&fakeRange{resp, "first", hold}, &fakeRange{resp, "second", nil}}

	go drainOutputs(context.Background(), tasks, true)
	if got, _ := nextChunk(t, resp, time.Second); got != "" {
		t.Fatalf("expected the leading empty chunk, got %q", got)
	}
	if got, ok := nextChunk(t, resp, time.Second); !ok || got != "second" {
		t.Fatalf("a later range must drain while an earlier one is still running, got %q ok=%v", got, ok)
	}
	close(hold)
	if got, ok := nextChunk(t, resp, time.Second); !ok || got != "first" {
		t.Fatalf("the held range must still be delivered, got %q ok=%v", got, ok)
	}
}

// TestDrainOutputsOrderedKeepsFileOrder guards client downloads, which must see
// bytes in file order.
func TestDrainOutputsOrderedKeepsFileOrder(t *testing.T) {
	resp := make(chan []byte, 8)
	hold := make(chan struct{})
	tasks := []common.DownloadTask{&fakeRange{resp, "first", hold}, &fakeRange{resp, "second", nil}}

	go drainOutputs(context.Background(), tasks, false)
	if got, _ := nextChunk(t, resp, time.Second); got != "" {
		t.Fatalf("expected the leading empty chunk, got %q", got)
	}
	if got, ok := nextChunk(t, resp, 200*time.Millisecond); ok {
		t.Fatalf("ordered drain delivered %q before the first range finished", got)
	}
	close(hold)
	for _, want := range []string{"first", "second"} {
		if got, ok := nextChunk(t, resp, time.Second); !ok || got != want {
			t.Fatalf("want %q in order, got %q ok=%v", want, got, ok)
		}
	}
}
