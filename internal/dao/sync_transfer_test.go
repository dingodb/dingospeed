package dao

import "testing"

func TestSyncRangeIsStrict(t *testing.T) {
	for _, raw := range []string{"bytes=+1-2", "bytes=-2", "bytes=1-", "bytes=0-1,3-4", "bytes=2-1", "bytes=0-10", "bytes= 1-2"} {
		if _, _, err := ParseSyncRange(raw, 10); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	a, b, err := ParseSyncRange("bytes=1-9", 10)
	if err != nil || a != 1 || b != 10 {
		t.Fatalf("valid range %d %d %v", a, b, err)
	}
}
