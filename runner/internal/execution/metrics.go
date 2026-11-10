package execution

import (
	"fmt"
	"io"
	"sync/atomic"
)

type Metrics struct {
	Active                                                                         atomic.Int64
	Finished, Failed, Cancelled, LeaseLost, ToolCalls, AgentRequests, UploadErrors atomic.Uint64
	InfrastructureFailures                                                         atomic.Uint64
	SessionsNanos, AgentNanos, QueueNanos, Claimed                                 atomic.Uint64
}

func (m *Metrics) Write(w io.Writer) {
	for _, stat := range []struct {
		name  string
		value uint64
	}{
		{"active_sessions", uint64(m.Active.Load())},
		{"finished_sessions_total", m.Finished.Load()},
		{"failed_sessions_total", m.Failed.Load()},
		{"infrastructure_failures_total", m.InfrastructureFailures.Load()},
		{"cancelled_sessions_total", m.Cancelled.Load()},
		{"lease_lost_total", m.LeaseLost.Load()},
		{"tool_calls_total", m.ToolCalls.Load()},
		{"agent_requests_total", m.AgentRequests.Load()},
		{"upload_errors_total", m.UploadErrors.Load()},
		{"session_duration_nanoseconds_total", m.SessionsNanos.Load()},
		{"agent_duration_nanoseconds_total", m.AgentNanos.Load()},
		{"claim_queue_duration_nanoseconds_total", m.QueueNanos.Load()},
		{"claimed_sessions_total", m.Claimed.Load()},
	} {
		fmt.Fprintf(w, "poligon_runner_%s %d\n", stat.name, stat.value)
	}
}
