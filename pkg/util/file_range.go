package util

import (
	"fmt"
	"strconv"
	"strings"
)

// FileRange converts a single HTTP byte range to the downloader's [start,end).
func FileRange(value string, size int64) (int64, int64, error) {
	if size < 0 {
		return 0, 0, fmt.Errorf("unknown file size")
	}
	if value == "" {
		return 0, size, nil
	}
	if size == 0 || !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, fmt.Errorf("unsupported byte range")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid byte range")
	}
	number := func(s string) (int64, error) {
		n, e := strconv.ParseInt(s, 10, 64)
		if e != nil || n < 0 {
			return 0, fmt.Errorf("invalid byte offset")
		}
		return n, nil
	}
	if parts[0] == "" {
		n, e := number(parts[1])
		if e != nil || n == 0 {
			return 0, 0, fmt.Errorf("invalid suffix range")
		}
		return max(0, size-n), size, nil
	}
	start, e := number(parts[0])
	if e != nil || start >= size {
		return 0, 0, fmt.Errorf("range out of bounds")
	}
	end := size
	if parts[1] != "" {
		last, e := number(parts[1])
		if e != nil || last < start {
			return 0, 0, fmt.Errorf("invalid range end")
		}
		if last < size-1 {
			end = last + 1
		}
	}
	return start, end, nil
}
