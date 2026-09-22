package dao

import (
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/dependency"
	"dingospeed/pkg/proto/manager"
	"dingospeed/pkg/util"
	"github.com/google/uuid"
)

var healthProcessID = uuid.NewString()
var healthStartedAt = time.Now().Unix()

// Only read existing in-memory observations. Never probe storage in a heartbeat.
func heartbeatHealth(now time.Time) *manager.NodeHealthSnapshot {
	period := config.SysConfig.Scheduler.Discovery.HeartbeatPeriod
	if period < 1 {
		period = 1
	}
	if period > 86400 {
		period = 86400
	}
	snapshot := &manager.NodeHealthSnapshot{
		Version: 1, ProcessId: healthProcessID, StartedAt: healthStartedAt,
		CollectedAt: max(now.Unix(), healthStartedAt), HeartbeatPeriodSeconds: uint32(period),
	}
	for _, capability := range []struct {
		id   dependency.ID
		name string
	}{
		{dependency.MetadataRead, "metadata_read"}, {dependency.MetadataWrite, "metadata_write"},
	} {
		observation := dependency.Default.Snapshot(capability.id, now)
		// Business observations can arrive after the caller captured now. Do not
		// produce a report whose observation timestamps exceed its collection time.
		snapshot.CollectedAt = max(snapshot.CollectedAt, healthUnix(observation.LastObservation), healthUnix(observation.LastFailure))
		snapshot.Capabilities = append(snapshot.Capabilities, &manager.CapabilityObservation{
			Capability: capability.name, State: int32(observation.State), Unresolved: observation.Unresolved,
			LastObservation: healthUnix(observation.LastObservation), LastFailure: healthUnix(observation.LastFailure),
		})
	}
	for _, observation := range util.FileAccessObservations() {
		snapshot.Errors = append(snapshot.Errors, &manager.StorageErrorCount{
			Operation: observation.Operation, Kind: string(observation.Kind), Count: observation.Count,
		})
	}
	return snapshot
}

func healthUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
