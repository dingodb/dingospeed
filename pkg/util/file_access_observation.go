package util

import (
	"dingospeed/pkg/dependency"
	"sync/atomic"
	"time"
)

var fileAccessOperations = [...]string{"stat", "read", "mkdir", "write"}
var fileAccessKinds = [...]FileAccessErrorKind{
	FileAccessPermissionDenied, FileAccessMountDisconnected, FileAccessIOFailure, FileAccessUnavailable,
}
var fileAccessCounts [4][4]atomic.Uint64

// ObserveFileAccessFailure records only filesystem failures already returned by
// an operation. It performs no I/O, logging, retries or background work, and
// retains no paths or error objects. Counts describe failed operations, not
// failed mounts or nodes. Unknown operations and content errors are ignored.
func ObserveFileAccessFailure(operation string, err error) {
	accessErr, ok := ClassifyFileAccessError("", err)
	if !ok {
		return
	}
	for i, op := range fileAccessOperations {
		if op != operation {
			continue
		}
		for j, kind := range fileAccessKinds {
			if kind == accessErr.Kind {
				fileAccessCounts[i][j].Add(1)
				id := dependency.MetadataRead
				if operation == "mkdir" || operation == "write" {
					id = dependency.MetadataWrite
				}
				dependency.Default.Observe(id, false, time.Now())
				return
			}
		}
	}
}

type FileAccessObservation struct {
	Operation string
	Kind      FileAccessErrorKind
	Count     uint64
}

// FileAccessObservations returns process-lifetime counters for diagnostics.
// Each count is atomic; the collection is not a transactional snapshot.
// The metrics collector exports these counts through the existing /metrics route.
func FileAccessObservations() []FileAccessObservation {
	result := make([]FileAccessObservation, 0, 16)
	for i, op := range fileAccessOperations {
		for j, kind := range fileAccessKinds {
			result = append(result, FileAccessObservation{op, kind, fileAccessCounts[i][j].Load()})
		}
	}
	return result
}
