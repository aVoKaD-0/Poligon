package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	env "poligon/environment/gen/environmentv1"
	"poligon/environment/pkg/api"
)

func (a *attempt) freeze() error {
	snapshot, err := json.Marshal(a.state.GetSnapshot())
	if err != nil {
		return err
	}
	id := a.state.SessionID
	now := time.Now().UTC()
	marker := api.AuditEvent{
		ID: fmt.Sprintf("final-%d", now.UnixNano()), Timestamp: now, SessionID: id,
		Action: "FINALIZE_ATTEMPT", EntityType: "session", EntityID: id, Method: "RPC", Path: env.EnvironmentService_FinalizeAttempt_FullMethodName, ResponseStatus: 200,
		AfterState: map[string]any{"schema_version": 1, "context": a.scope, "environment_session_id": id, "snapshot_sha256": digest(snapshot), "snapshot_finalized": true, "audit_complete": true},
	}
	// Keep the HTTP audit and frozen NDJSON in agreement, including on serialization errors.
	var audit bytes.Buffer
	enc := json.NewEncoder(&audit)
	for _, event := range append(a.state.GetAuditLog(), marker) {
		if err = enc.Encode(event); err != nil {
			return err
		}
	}
	a.state.AppendAudit(marker)
	a.snapshot = newArtifact(a.scope, "environment_snapshot", "application/json", snapshot)
	a.audit = newArtifact(a.scope, "environment_audit", "application/x-ndjson", audit.Bytes())
	return nil
}

func (s *Server) FinalizeAttempt(ctx context.Context, r *env.FinalizeAttemptRequest) (*env.FinalizeAttemptResponse, error) {
	scope, id := scopeOf(r.GetContext()), r.GetEnvironmentSessionId()
	if !scope.valid() || !crmID.MatchString(id) {
		return nil, status.Error(codes.InvalidArgument, "Invalid execution context or CRM id")
	}
	a := s.lookup(scope, id)
	if a == nil {
		return nil, status.Error(codes.PermissionDenied, "Unknown attempt binding")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case <-a.seal():
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	select {
	case a.publishing <- struct{}{}:
		defer func() { <-a.publishing }()
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if a.snapshot == nil {
		if err := a.freeze(); err != nil {
			return nil, status.Error(codes.Internal, "Could not serialize CRM artifacts")
		}
	}
	for _, part := range []*artifact{a.snapshot, a.audit} {
		if err := s.uploader.publish(ctx, part); err != nil {
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			code := status.Code(err)
			if code == codes.Unknown {
				code = codes.Unavailable
			}
			return nil, status.Error(code, err.Error())
		}
	}
	return &env.FinalizeAttemptResponse{Context: scope.proto(), EnvironmentSessionId: id, SnapshotRef: a.snapshot.ref, AuditRef: a.audit.ref, SnapshotFinalized: true, AuditComplete: true}, nil
}
