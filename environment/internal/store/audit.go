package store

import (
	"encoding/json"
	"fmt"

	"poligon/environment/internal/domain"
)

// snapshotAuditEvent freezes arbitrary payloads as JSON and gives each caller
// its own byte slices. Caller-owned pointers must never rewrite past evidence.
func snapshotAuditEvent(event domain.AuditEvent) domain.AuditEvent {
	for _, field := range []struct {
		name  string
		value *any
	}{
		{"request_payload", &event.RequestPayload},
		{"before_state", &event.BeforeState},
		{"after_state", &event.AfterState},
	} {
		if *field.value == nil {
			continue
		}
		data, err := json.Marshal(*field.value)
		if err != nil {
			*field.value = nil
			if event.ErrorMessage != "" {
				event.ErrorMessage += "; "
			}
			event.ErrorMessage += fmt.Sprintf("cannot snapshot %s: %v", field.name, err)
			continue
		}
		*field.value = json.RawMessage(data)
	}
	return event
}
