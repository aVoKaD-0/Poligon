package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/poligon/runner/contracts"
	pb "github.com/poligon/runner/contracts/controlv1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Disposition int

const (
	Retry Disposition = iota
	Ack
)

type Engine struct {
	Control           Control
	Publisher         Publisher
	Evidence          Evidence
	Environment       Environment
	Agents            AgentFactory
	Simulators        SimulatorFactory
	Schemas           contracts.Schemas
	Metrics           *Metrics
	Log               *slog.Logger
	TempDir           string
	EvidenceLimit     int64
	PlanLimit         int
	HeartbeatInterval time.Duration
	CloseTimeout      time.Duration
}

type failure struct {
	code      string
	retryable bool
}

func (e *Engine) Run(parent context.Context, job Job) Disposition {
	ctx := propagation.TraceContext{}.Extract(parent, propagation.MapCarrier{"traceparent": job.Traceparent})
	ctx, span := otel.Tracer("poligon/runner").Start(ctx, "session.execute", trace.WithAttributes(
		attribute.String("poligon.project_id", job.ProjectID), attribute.String("poligon.run_id", job.RunID),
		attribute.String("poligon.session_id", job.SessionID), attribute.Int64("poligon.attempt_no", int64(job.Attempt))))
	defer span.End()
	started := time.Now()
	worker := uuid.NewString()
	log := e.Log.With("project_id", job.ProjectID, "run_id", job.RunID, "session_id", job.SessionID,
		"attempt_no", job.Attempt, "dispatch_id", job.Payload.Dispatch, "correlation_id", job.RunID,
		"trace_id", span.SpanContext().TraceID().String(), "worker_id", worker)
	req := &pb.LeaseRequest{Context: job.Scope.Proto(), WorkerId: worker, DispatchId: job.Payload.Dispatch}
	response, err := e.Control.Claim(ctx, req)
	if err != nil {
		log.Warn("claim RPC failed")
		return Retry
	}
	if !response.GetAccepted() {
		log.Info("execution claim declined")
		switch response.GetReason() {
		case "STALE_ATTEMPT", "CANCEL_REQUESTED":
			return Ack
		default:
			return Retry
		}
	}
	until := expiry(response)
	if !IDValid(response.GetLeaseId()) || !until.After(time.Now()) {
		return Retry
	}
	req.LeaseId = response.GetLeaseId()
	e.Metrics.Active.Add(1)
	e.Metrics.Claimed.Add(1)
	if age := time.Since(job.OccurredAt); age > 0 {
		e.Metrics.QueueNanos.Add(uint64(age))
	}
	defer func() {
		e.Metrics.Active.Add(-1)
		e.Metrics.SessionsNanos.Add(uint64(time.Since(started)))
	}()
	var clock atomic.Int64
	clock.Store(until.UnixNano())
	lease, stopLease := context.WithCancelCause(context.WithValue(ctx, leaseClockKey{}, &clock))
	work, stopWork := context.WithCancelCause(lease)
	if response.GetCancelRequested() {
		stopWork(errCancelled)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitorLease(lease, e.Control, req, until, e.HeartbeatInterval, &clock, stopLease, stopWork, e.Metrics)
	}()
	defer func() { stopWork(nil); stopLease(nil); <-done }()

	ev := newEvents(ctx, job, e.Publisher)
	access := Access{job.Scope, worker, req.LeaseId}
	if context.Cause(work) != errCancelled {
		if err := ev.state(lease, "session.started.v1", map[string]any{"worker_id": worker}); err != nil {
			return e.terminal(lease, work, ev, log, "infrastructure_error", Ref{}, nil, failure{"TRANSPORT_ERROR", true})
		}
	}
	if CheckLease(work) != nil {
		return e.terminal(lease, work, ev, log, "cancelled", Ref{}, nil, failure{})
	}
	if response.GetRuntimePlanRef() != job.Payload.PlanRef || response.GetRuntimePlanSha256() != job.Payload.PlanHash {
		return e.terminal(lease, work, ev, log, "infrastructure_error", Ref{}, nil, failure{"RUNTIME_PLAN_INVALID", false})
	}
	frozen, err := e.Control.Plan(work, &pb.PlanRequest{
		Context: req.Context, WorkerId: worker, LeaseId: req.LeaseId,
		RuntimePlanRef: job.Payload.PlanRef, RuntimePlanSha256: job.Payload.PlanHash,
	})
	if err != nil {
		return e.terminal(lease, work, ev, log, "infrastructure_error", Ref{}, nil, failure{"DEPENDENCY_UNAVAILABLE", true})
	}
	if frozen.GetCancelRequested() {
		stopWork(errCancelled)
		return e.terminal(lease, work, ev, log, "cancelled", Ref{}, nil, failure{})
	}
	plan, err := ParsePlan(job, frozen, e.Schemas, e.PlanLimit)
	if err != nil {
		return e.terminal(lease, work, ev, log, "infrastructure_error", Ref{}, nil, failure{"RUNTIME_PLAN_INVALID", false})
	}
	var secret string
	if plan.CredentialRef != "" {
		secret = strings.Trim(frozen.GetRuntimeCredentials()["agent"], " ")
		if secret == "" {
			return e.terminal(lease, work, ev, log, "infrastructure_error", Ref{}, nil, failure{"CREDENTIAL_UNAVAILABLE", false})
		}
	}
	if e.Simulators == nil {
		return e.terminal(lease, work, ev, log, "infrastructure_error", Ref{}, nil, failure{"DEPENDENCY_UNAVAILABLE", false})
	}
	sessionEnd := started.Add(time.Duration(plan.Limits.Duration) * time.Millisecond)
	session, stopSession := context.WithDeadlineCause(work, sessionEnd, errSessionDeadline)
	defer stopSession()

	conversation, err := openJournal(e.TempDir)
	if err != nil {
		return e.terminal(lease, work, ev, log, "infrastructure_error", Ref{}, nil, failure{"INFRASTRUCTURE_ERROR", true})
	}
	defer conversation.close()
	tools, err := openJournal(e.TempDir)
	if err != nil {
		return e.terminal(lease, work, ev, log, "infrastructure_error", Ref{}, nil, failure{"INFRASTRUCTURE_ERROR", true})
	}
	defer tools.close()
	rec := &recorder{events: ev, conversation: conversation, tools: tools, secret: secret, limit: e.EvidenceLimit}
	state := dialogue{engine: e, rec: rec, plan: plan, callIDs: make(map[string]struct{}, plan.Limits.Tools)}
	workspace, reason, issue := state.interact(session, lease, secret)
	stopSession()
	if CheckLease(lease) != nil {
		return Retry
	}
	if context.Cause(work) == errCancelled {
		reason = "cancelled"
	}
	finished := time.Now().UTC()
	if err := rec.append(lease, conversation, "session.ended", map[string]string{"termination_reason": reason}); err != nil && issue.code == "" {
		issue = failure{journalError(err), !errors.Is(err, ErrLimit) && reason != "agent_protocol_error"}
	}
	final, stopFinal := context.WithDeadline(lease, minTime(time.Now().Add(e.CloseTimeout), sessionEnd.Add(25*time.Second)))
	defer stopFinal()
	type finalization struct {
		snapshot, audit Ref
		err             error
	}
	var environmentResult chan finalization
	if workspace != nil && CheckLease(final) == nil {
		environmentResult = make(chan finalization, 1)
		// Seal the CRM workspace independently of potentially slow journal PUTs.
		go func() {
			snapshot, audit, err := workspace.Finalize(final)
			environmentResult <- finalization{snapshot, audit, err}
		}()
	}
	var refs []Ref
	for _, source := range []struct {
		journal *journal
		kind    string
	}{{conversation, "conversation"}, {tools, "tool_calls"}} {
		blob, err := source.journal.blob(source.kind)
		if err == nil && CheckLease(final) == nil {
			var ref Ref
			ref, err = e.Evidence.Upload(final, access, blob)
			if err == nil && ref.Valid(source.kind) {
				refs = append(refs, ref)
				continue
			}
		}
		e.Metrics.UploadErrors.Add(1)
		if issue.code == "" {
			issue = failure{"ARTIFACT_UPLOAD_FAILED", reason != "agent_protocol_error"}
		}
	}
	if rec.missed > 0 {
		log.Warn("live progress missing from broker", "records", rec.missed)
	}
	var manifest Ref
	if environmentResult != nil && CheckLease(final) == nil {
		result := finalization{err: context.DeadlineExceeded}
		select {
		case result = <-environmentResult:
		case <-final.Done():
		}
		snapshot, audit, err := result.snapshot, result.audit, result.err
		if err == nil && snapshot.Valid("environment_snapshot") && audit.Valid("environment_audit") {
			err = e.Evidence.Resolve(final, access, snapshot)
			if err == nil {
				err = e.Evidence.Resolve(final, access, audit)
			}
			if err == nil {
				if len(refs) == 2 {
					status := "executed"
					if issue.code != "" {
						status = "failed"
					}
					if context.Cause(work) == errCancelled {
						status, reason = "cancelled", "cancelled"
					}
					manifest, err = e.manifest(final, access, plan, state, status, reason, started.UTC(), finished, refs[0], refs[1], snapshot, audit)
					if err != nil && issue.code == "" {
						issue = failure{"ARTIFACT_UPLOAD_FAILED", reason != "agent_protocol_error"}
					}
				}
				if !manifest.Valid("execution_manifest") {
					refs = append(refs, snapshot, audit)
				}
			}
		} else if err == nil {
			err = ErrDependency
		}
		if err != nil && issue.code == "" {
			issue = failure{"DEPENDENCY_UNAVAILABLE", !errors.Is(err, ErrUnsupported) && reason != "agent_protocol_error"}
		}
	}
	if manifest.Valid("execution_manifest") {
		refs = append(refs, manifest)
	}
	if issue.code == "" && reason != "cancelled" && !manifest.Valid("execution_manifest") {
		issue = failure{"DEPENDENCY_UNAVAILABLE", false}
	}
	return e.terminal(lease, work, ev, log, reason, manifest, refs, issue)
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func (e *Engine) terminal(lease, work context.Context, ev *events, log *slog.Logger,
	reason string, manifest Ref, refs []Ref, issue failure) Disposition {
	if err := CheckLease(lease); err != nil {
		log.Warn("execution interrupted", "lease_lost", errors.Is(err, errLease))
		return Retry
	}
	kind := "session.executed.v1"
	payload := map[string]any{"termination_reason": reason}
	if len(refs) > 0 {
		payload["evidence_refs"] = refs
	}
	switch {
	case context.Cause(work) == errCancelled:
		kind = "session.cancelled.v1"
		payload["termination_reason"] = "cancelled"
	case issue.code != "":
		kind = "session.failed.v1"
		payload["error_code"] = issue.code
		payload["retryable"] = issue.retryable
		if reason != "agent_protocol_error" {
			payload["termination_reason"] = "infrastructure_error"
		}
	default:
		if !manifest.Valid("execution_manifest") {
			return Retry
		}
		payload["manifest_ref"] = manifest
		payload["manifest_sha256"] = manifest.SHA
	}
	if err := ev.state(lease, kind, payload); err != nil {
		log.Warn("terminal event not acknowledged", "type", kind)
		return Retry
	}
	switch kind {
	case "session.executed.v1":
		e.Metrics.Finished.Add(1)
	case "session.cancelled.v1":
		e.Metrics.Cancelled.Add(1)
	case "session.failed.v1":
		e.Metrics.Failed.Add(1)
		if payload["termination_reason"] == "infrastructure_error" {
			e.Metrics.InfrastructureFailures.Add(1)
		}
	}
	log.Info("terminal event acknowledged", "type", kind, "reason", payload["termination_reason"], "error_code", issue.code)
	return Ack
}

func (e *Engine) manifest(ctx context.Context, access Access, p Plan, d dialogue, status, reason string,
	started, finished time.Time, conversation, tools, snapshot, audit Ref) (Ref, error) {
	usage := map[string]any{"client_turns": d.turns, "tool_calls": d.toolCalls, "cost_status": "unknown"}
	if d.costReports > 0 && (d.rec.secret == "" || !strings.Contains(strconv.FormatInt(d.cost, 10), d.rec.secret)) {
		usage["reported_cost_minor"] = d.cost
		usage["cost_status"] = "partial"
		if d.costReports == d.replies && !d.incomplete {
			usage["cost_status"] = "reported"
		}
	}
	raw, err := json.Marshal(struct {
		Scope
		Schema       int                  `json:"schema_version"`
		Versions     Versions             `json:"versions"`
		Seed         int64                `json:"seed"`
		Status       string               `json:"execution_status"`
		Reason       string               `json:"termination_reason"`
		Conversation Ref                  `json:"conversation_ref"`
		Tools        Ref                  `json:"tool_calls_ref"`
		Snapshot     Ref                  `json:"environment_snapshot_ref"`
		Audit        Ref                  `json:"environment_audit_ref"`
		Audio        []Ref                `json:"audio_refs"`
		Usage        map[string]any       `json:"usage"`
		Timestamps   map[string]time.Time `json:"timestamps"`
	}{p.Scope, 1, p.Versions, *p.Seed, status, reason, conversation, tools, snapshot, audit, []Ref{}, usage,
		map[string]time.Time{"started_at": started, "finished_at": finished}})
	if err != nil {
		return Ref{}, err
	}
	if err = contracts.Validate(e.Schemas.Manifest, raw); err != nil {
		return Ref{}, err
	}
	sum := sha256.Sum256(raw)
	ref, err := e.Evidence.Upload(ctx, access, Blob{"execution_manifest", "application/json", hex.EncodeToString(sum[:]), int64(len(raw)), bytes.NewReader(raw)})
	if err != nil {
		e.Metrics.UploadErrors.Add(1)
	}
	return ref, err
}
