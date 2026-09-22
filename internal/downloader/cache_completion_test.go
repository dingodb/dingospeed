package downloader

import (
	"context"
	"dingospeed/pkg/config"
	"path/filepath"
	"testing"
)

func TestCacheCompletionRequiresWholeReadableFile(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	config.SysConfig = &config.Config{}
	for _, tc := range []struct {
		name              string
		start, end        int64
		missing, canceled bool
		want              int
	}{
		{name: "complete", end: 7, want: 1},
		{name: "prefix", end: 4},
		{name: "suffix", start: 4, end: 7},
		{name: "missing_tail", end: 7, missing: true},
		{name: "canceled", end: 7, canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, err := NewDingCache(filepath.Join(t.TempDir(), "blob"), 4)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			if err = cache.Resize(7); err != nil {
				t.Fatal(err)
			}
			if err = cache.WriteBlock(0, []byte("abcd")); err != nil {
				t.Fatal(err)
			}
			if !tc.missing {
				if err = cache.WriteBlock(1, []byte("efg\x00")); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			task := NewCacheFileTask(0, tc.start, tc.end)
			task.Context, task.DingFile = ctx, cache
			task.ResponseChan = make(chan []byte, 4)
			calls := 0
			task.OnComplete = func() { calls++ }
			task.OutResult()
			if calls != tc.want {
				t.Fatalf("completion calls=%d want=%d", calls, tc.want)
			}
		})
	}
}
