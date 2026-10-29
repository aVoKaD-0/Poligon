package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
)

type Envelope struct {
	Traceparent     string    `json:"traceparent"`
	SchemaVersion   int       `json:"schema_version"`
	MessageID       string    `json:"message_id"`
	ProducerEventID string    `json:"producer_event_id"`
	Type            string    `json:"type"`
	Producer        string    `json:"producer"`
	OccurredAt      time.Time `json:"occurred_at"`
	ProjectID       string    `json:"project_id"`
	RunID           string    `json:"run_id"`
	SessionID       string    `json:"session_id"`
	AttemptNo       int       `json:"attempt_no"`
	Payload         Object    `json:"payload"`
}

func parseEvent(subject string, b []byte) (Envelope, error) {
	var v Envelope
	if len(b) > 64<<10 {
		return v, errors.New("event exceeds 64 KiB")
	}
	if e := json.Unmarshal(b, &v); e != nil {
		return v, e
	}
	allowed := map[string]string{"session.started.v1": "runner", "session.progress.v1": "runner", "session.executed.v1": "runner", "session.failed.v1": "runner", "session.cancelled.v1": "runner", "evaluation.completed.v1": "intelligence", "evaluation.failed.v1": "intelligence", "evaluation.cancelled.v1": "intelligence"}
	if v.SchemaVersion != 1 || allowed[v.Type] != v.Producer || v.Producer == "" || subject != "poligon.evt."+v.Type || !validID(v.MessageID) || !validID(v.ProducerEventID) || !validID(v.ProjectID) || !validID(v.RunID) || !validID(v.SessionID) || v.AttemptNo < 1 || v.OccurredAt.IsZero() || v.Payload == nil {
		return v, errors.New("invalid event envelope")
	}
	if strings.HasPrefix(v.Type, "session.") && integer(v.Payload, "session_seq", 0) < 1 {
		return v, errors.New("session_seq required")
	}
	if strings.HasPrefix(v.Type, "evaluation.") && !validID(str(v.Payload, "evaluation_id")) {
		return v, errors.New("evaluation_id required")
	}
	return v, nil
}
func (a *App) ApplyEvent(ctx context.Context, subject string, b []byte) error {
	v, e := parseEvent(subject, b)
	if e != nil {
		return invalid(e.Error())
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": v.Traceparent})
	ctx = context.WithValue(ctx, ctxKey("causation_id"), v.MessageID)
	var existing string
	err := a.DB.Pool.QueryRow(ctx, "SELECT payload_sha256 FROM inbox WHERE message_id=$1 OR(producer=$2 AND producer_event_id=$3) LIMIT 1", v.MessageID, v.Producer, v.ProducerEventID).Scan(&existing)
	if err == nil {
		if existing != hash(b) {
			return conflict("Message identity reused with different payload")
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var manifest, assessment Object
	if v.Type == "session.executed.v1" {
		manifest, e = a.readEvidence(ctx, v, sub(v.Payload, "manifest_ref"), "execution_manifest", str(v.Payload, "manifest_sha256"))
		if e != nil {
			return e
		}
		if e = a.validateManifest(ctx, v, manifest); e != nil {
			return e
		}
		if e = a.rebuildProjection(ctx, v, manifest); e != nil {
			return e
		}
	}
	if v.Type == "evaluation.completed.v1" {
		assessment, e = a.readEvidence(ctx, v, sub(v.Payload, "assessment_ref"), "assessment", str(v.Payload, "assessment_sha256"))
		if e != nil {
			return e
		}
		if e = validateArtifactSchema("assessment", assessment); e != nil {
			return e
		}
		if e = ValidateAssessment(assessment); e != nil {
			return e
		}
		if str(assessment, "verdict") != str(v.Payload, "verdict") {
			return invalid("Assessment verdict mismatch")
		}
	}
	return a.DB.Tx(ctx, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, "INSERT INTO inbox(message_id,producer,producer_event_id,payload_sha256) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", v.MessageID, v.Producer, v.ProducerEventID, hash(b))
		if e != nil {
			return e
		}
		if tag.RowsAffected() == 0 {
			var original string
			if e = tx.QueryRow(ctx, "SELECT payload_sha256 FROM inbox WHERE message_id=$1 OR(producer=$2 AND producer_event_id=$3) LIMIT 1", v.MessageID, v.Producer, v.ProducerEventID).Scan(&original); e != nil {
				return e
			}
			if original != hash(b) {
				return conflict("Message identity reused with different payload")
			}
			return nil
		}
		s, e := getSession(ctx, tx, v.ProjectID, v.SessionID)
		if e != nil {
			return e
		}
		if str(s, "run_id") != v.RunID {
			return invalid("Event run mismatch")
		}
		stale := func(reason string) error {
			_, e := tx.Exec(ctx, "INSERT INTO stale_results(message_id,context,reason) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", v.MessageID, encode(v), reason)
			return e
		}
		if integer(s, "current_attempt_no", 0) != v.AttemptNo {
			return stale("STALE_ATTEMPT")
		}
		var cancelled bool
		var runStatus string
		if e = tx.QueryRow(ctx, "SELECT cancel_requested,status FROM runs WHERE id=$1", v.RunID).Scan(&cancelled, &runStatus); e != nil {
			return e
		}
		if strings.HasPrefix(v.Type, "session.") {
			at, e := oneJSON(ctx, tx, "SELECT to_jsonb(t) FROM execution_attempts t WHERE session_id=$1 AND attempt_no=$2", v.SessionID, v.AttemptNo)
			if e != nil {
				return e
			}
			active := str(at, "status") == "leased" || str(at, "status") == "preparing" || str(at, "status") == "running"
			expiry, _ := time.Parse(time.RFC3339Nano, str(at, "lease_expiry"))
			if !active || !expiry.After(utc()) {
				return stale("EXECUTION_INACTIVE_OR_EXPIRED")
			}
			if terminal(str(s, "status")) {
				return stale("SESSION_TERMINAL")
			}
			seq := integer(v.Payload, "session_seq", 0)
			if seq <= integer(at, "last_seq", 0) {
				if v.Type == "session.progress.v1" || v.Type == "session.started.v1" {
					return stale("OLD_SEQUENCE")
				}
			}
			if _, e = tx.Exec(ctx, "UPDATE execution_attempts SET last_seq=greatest(last_seq,$3) WHERE session_id=$1 AND attempt_no=$2", v.SessionID, v.AttemptNo, seq); e != nil {
				return e
			}
			switch v.Type {
			case "session.started.v1":
				if str(v.Payload, "worker_id") != str(at, "worker_id") {
					return invalid("Worker mismatch")
				}
				_, e = tx.Exec(ctx, "UPDATE execution_attempts SET status='running' WHERE session_id=$1 AND attempt_no=$2", v.SessionID, v.AttemptNo)
			case "session.progress.v1":
				_, e = tx.Exec(ctx, "INSERT INTO progress_events(project_id,run_id,session_id,attempt_no,session_seq,producer_event_id,payload) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING", v.ProjectID, v.RunID, v.SessionID, v.AttemptNo, seq, v.ProducerEventID, encode(v.Payload))
				if e != nil {
					return e
				}
				return emit(ctx, tx, v.RunID, "session.progress", Object{"session_id": v.SessionID, "attempt_no": v.AttemptNo, "session_seq": seq, "event": v.Payload})
			case "session.executed.v1":
				if cancelled {
					return stale("CANCEL_REQUESTED")
				}
				var plan []byte
				if e = tx.QueryRow(ctx, "SELECT plan FROM execution_attempts WHERE session_id=$1 AND attempt_no=$2", v.SessionID, v.AttemptNo).Scan(&plan); e != nil {
					return e
				}
				frozen := obj(plan)
				_, mh, _ := canonical(manifest["versions"])
				_, ph, _ := canonical(frozen["versions"])
				if mh != ph || stringNumber(manifest["seed"]) != stringNumber(frozen["seed"]) {
					return invalid("Manifest frozen inputs mismatch")
				}
				_, e = tx.Exec(ctx, "UPDATE execution_attempts SET status='executed',manifest_ref=$3,manifest=$4,lease_expiry=NULL WHERE session_id=$1 AND attempt_no=$2", v.SessionID, v.AttemptNo, encode(v.Payload["manifest_ref"]), encode(manifest))
				if e != nil {
					return e
				}
				_, e = addEvaluation(ctx, tx, s, str(sub(frozen, "versions"), "evaluator_version_id"), "primary", "", 1)
				if e != nil {
					return e
				}
				_, e = tx.Exec(ctx, "UPDATE sessions SET completeness=$2 WHERE id=$1", v.SessionID, encode(Object{"status": "complete", "manifest_ref": v.Payload["manifest_ref"]}))
			case "session.failed.v1":
				retry, _ := v.Payload["retryable"].(bool)
				e = a.failExecution(ctx, tx, s, str(v.Payload, "error_code"), retry, cancelled)
			case "session.cancelled.v1":
				if !cancelled {
					return invalid("Unexpected cancellation")
				}
				_, e = tx.Exec(ctx, "UPDATE execution_attempts SET status='cancelled',lease_expiry=NULL WHERE session_id=$1 AND attempt_no=$2", v.SessionID, v.AttemptNo)
				if e != nil {
					return e
				}
				_, e = tx.Exec(ctx, "UPDATE sessions SET status='cancelled',verdict=NULL WHERE id=$1", v.SessionID)
			}
			if e != nil {
				return e
			}
		} else {
			jobID := str(v.Payload, "evaluation_id")
			job, e := oneJSON(ctx, tx, "SELECT to_jsonb(e) FROM evaluation_jobs e WHERE id=$1 AND session_id=$2 AND attempt_no=$3", jobID, v.SessionID, v.AttemptNo)
			if e != nil {
				return e
			}
			current, _ := job["current"].(bool)
			expiry, _ := time.Parse(time.RFC3339Nano, str(job, "lease_expiry"))
			if !current || str(job, "status") != "running" || !expiry.After(utc()) {
				return stale("EVALUATION_INACTIVE_OR_EXPIRED")
			}
			ownCancel, _ := job["cancel_requested"].(bool)
			cancelled = cancelled || ownCancel
			if str(job, "purpose") == "manual" && terminal(runStatus) {
				cancelled = ownCancel
			}
			switch v.Type {
			case "evaluation.completed.v1":
				if cancelled {
					return stale("CANCEL_REQUESTED")
				}
				var manifestSha string
				if e = tx.QueryRow(ctx, "SELECT manifest_ref->>'sha256' FROM execution_attempts WHERE session_id=$1 AND attempt_no=$2", v.SessionID, v.AttemptNo).Scan(&manifestSha); e != nil {
					return e
				}
				if str(assessment, "manifest_sha256") != manifestSha {
					return invalid("Assessment source manifest hash mismatch")
				}
				if str(assessment, "evaluation_id") != jobID || str(assessment, "evaluator_version_id") != str(job, "evaluator_version_id") {
					return invalid("Assessment job/version mismatch")
				}
				_, e = tx.Exec(ctx, "UPDATE evaluation_jobs SET status='completed',assessment_ref=$2,assessment=$3,lease_expiry=NULL WHERE id=$1", jobID, encode(v.Payload["assessment_ref"]), encode(assessment))
				if e != nil {
					return e
				}
				if str(job, "purpose") == "primary" {
					if str(s, "current_evaluation_id") != jobID || s["primary_evaluation_id"] != nil {
						return stale("PRIMARY_ALREADY_FIXED")
					}
					_, e = tx.Exec(ctx, "UPDATE sessions SET status='completed',verdict=$2,primary_evaluation_id=$3,error=NULL WHERE id=$1", v.SessionID, str(assessment, "verdict"), jobID)
					if e != nil {
						return e
					}
				}
				if e = emit(ctx, tx, v.RunID, "assessment.ready", Object{"session_id": v.SessionID, "evaluation_id": jobID, "purpose": job["purpose"]}); e != nil {
					return e
				}
			case "evaluation.failed.v1":
				retry, _ := v.Payload["retryable"].(bool)
				e = a.failEvaluation(ctx, tx, s, job, str(v.Payload, "error_code"), retry, cancelled)
			case "evaluation.cancelled.v1":
				if !cancelled {
					return invalid("Unexpected cancellation")
				}
				e = a.failEvaluation(ctx, tx, s, job, "CANCELLED", false, true)
			}
			if e != nil {
				return e
			}
			if str(job, "purpose") == "manual" {
				return nil
			}
		}
		fresh, e := getSession(ctx, tx, v.ProjectID, v.SessionID)
		if e != nil {
			return e
		}
		if e = emit(ctx, tx, v.RunID, "session.updated", fresh); e != nil {
			return e
		}
		return aggregate(ctx, tx, v.RunID)
	})
}
func retryableInfra(code string) bool {
	switch code {
	case "LEASE_EXPIRED", "INFRASTRUCTURE_ERROR", "TRANSPORT_ERROR", "ARTIFACT_UPLOAD_FAILED", "DEPENDENCY_UNAVAILABLE":
		return true
	}
	return false
}
func (a *App) failExecution(ctx context.Context, tx pgx.Tx, s Object, code string, retry, cancel bool) error {
	sid := str(s, "id")
	n := integer(s, "current_attempt_no", 1)
	status := "failed"
	if code == "LEASE_EXPIRED" {
		status = "expired"
	}
	if cancel {
		status = "cancelled"
	}
	_, e := tx.Exec(ctx, "UPDATE execution_attempts SET status=$3,lease_expiry=NULL WHERE session_id=$1 AND attempt_no=$2", sid, n, status)
	if e != nil {
		return e
	}
	if !cancel && retry && retryableInfra(code) && n < 2 {
		var b []byte
		if e = tx.QueryRow(ctx, "SELECT plan FROM execution_attempts WHERE session_id=$1 AND attempt_no=$2", sid, n).Scan(&b); e != nil {
			return e
		}
		if e = addAttempt(ctx, tx, str(s, "project_id"), str(s, "run_id"), sid, n+1, obj(b)); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, "UPDATE sessions SET status='queued',current_attempt_no=$2,error=NULL WHERE id=$1", sid, n+1)
		return e
	}
	status = "failed"
	if cancel {
		status = "cancelled"
	}
	_, e = tx.Exec(ctx, "UPDATE sessions SET status=$2,verdict=NULL,error=$3 WHERE id=$1", sid, status, encode(Object{"code": code}))
	return e
}
func (a *App) failEvaluation(ctx context.Context, tx pgx.Tx, s, job Object, code string, retry, cancel bool) error {
	status := "failed"
	if cancel {
		status = "cancelled"
	}
	_, e := tx.Exec(ctx, "UPDATE evaluation_jobs SET status=$2,lease_expiry=NULL,error=$3 WHERE id=$1", str(job, "id"), status, encode(Object{"code": code}))
	if e != nil {
		return e
	}
	if !cancel && retry && retryableInfra(code) && integer(job, "retry_index", 1) < 3 {
		if _, e = tx.Exec(ctx, "UPDATE evaluation_jobs SET current=false WHERE id=$1", str(job, "id")); e != nil {
			return e
		}
		_, e = addEvaluation(ctx, tx, s, str(job, "evaluator_version_id"), str(job, "purpose"), str(job, "evaluation_group_id"), integer(job, "retry_index", 1)+1)
		return e
	}
	if str(job, "purpose") == "primary" {
		_, e = tx.Exec(ctx, "UPDATE sessions SET status=$2,verdict=NULL,error=$3 WHERE id=$1", str(s, "id"), status, encode(Object{"code": code}))
	}
	return e
}
func (a *App) Supervise(ctx context.Context) error {
	return a.DB.Tx(ctx, func(tx pgx.Tx) error {
		attempts, e := queryJSON(ctx, tx, `SELECT to_jsonb(s)||jsonb_build_object('cancel',r.cancel_requested,'queue_timeout',a.status='queued') FROM execution_attempts a JOIN sessions s ON s.id=a.session_id JOIN runs r ON r.id=s.run_id WHERE a.attempt_no=s.current_attempt_no AND ((a.status IN('leased','preparing','running') AND (a.lease_expiry<=now() OR a.deadline<=now())) OR (a.status='queued' AND a.created_at+ r.queue_timeout_ms*interval '1 millisecond' <=now()))`)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, "DELETE FROM web_sessions WHERE expires_at<now(); DELETE FROM login_states WHERE expires_at<now(); DELETE FROM http_idempotency WHERE expires_at<now(); DELETE FROM run_events WHERE created_at<now()-interval '30 days' AND run_id IN(SELECT id FROM runs WHERE status IN('completed','failed','cancelled'))")
		if e != nil {
			return e
		}
		changed := map[string]bool{}
		for _, s := range attempts {
			cancel, _ := s["cancel"].(bool)
			queued, _ := s["queue_timeout"].(bool)
			code := "LEASE_EXPIRED"
			if queued {
				code = "QUEUE_TIMEOUT"
			}
			if e = a.failExecution(ctx, tx, s, code, !queued, cancel); e != nil {
				return e
			}
			changed[str(s, "run_id")] = true
		}
		jobs, e := queryJSON(ctx, tx, `SELECT to_jsonb(e)||jsonb_build_object('run_cancel',CASE WHEN e.purpose='manual' AND r.status IN('completed','failed','cancelled') THEN false ELSE r.cancel_requested END,'project_id',s.project_id,'run_id',s.run_id,'current_attempt_no',s.current_attempt_no) FROM evaluation_jobs e JOIN sessions s ON s.id=e.session_id JOIN runs r ON r.id=s.run_id WHERE e.current AND ((e.status='running' AND (e.lease_expiry<=now() OR e.deadline<=now())) OR(e.status='queued' AND e.created_at+r.queue_timeout_ms*interval '1 millisecond'<=now()))`)
		if e != nil {
			return e
		}
		for _, job := range jobs {
			s, e := getSession(ctx, tx, str(job, "project_id"), str(job, "session_id"))
			if e != nil {
				return e
			}
			cancel, _ := job["run_cancel"].(bool)
			own, _ := job["cancel_requested"].(bool)
			code := "LEASE_EXPIRED"
			if str(job, "status") == "queued" {
				code = "QUEUE_TIMEOUT"
			}
			if e = a.failEvaluation(ctx, tx, s, job, code, code != "QUEUE_TIMEOUT", cancel || own); e != nil {
				return e
			}
			if str(job, "purpose") == "primary" {
				changed[str(job, "run_id")] = true
			}
		}
		for id := range changed {
			if e = aggregate(ctx, tx, id); e != nil {
				return e
			}
		}
		return nil
	})
}
func SetupStreams(js nats.JetStreamContext) error {
	for _, s := range []*nats.StreamConfig{{Name: "POLIGON_EXECUTION", Subjects: []string{"poligon.cmd.session.>"}, Retention: nats.WorkQueuePolicy, Storage: nats.FileStorage, MaxAge: 7 * 24 * time.Hour, MaxBytes: 1 << 30, Discard: nats.DiscardNew}, {Name: "POLIGON_EVALUATION", Subjects: []string{"poligon.cmd.evaluation.>"}, Retention: nats.WorkQueuePolicy, Storage: nats.FileStorage, MaxAge: 7 * 24 * time.Hour, MaxBytes: 1 << 30, Discard: nats.DiscardNew}, {Name: "POLIGON_EVENTS", Subjects: []string{"poligon.evt.>"}, Retention: nats.LimitsPolicy, Storage: nats.FileStorage, MaxAge: 30 * 24 * time.Hour, MaxBytes: 4 << 30, Discard: nats.DiscardNew}} {
		if _, e := js.StreamInfo(s.Name); e != nil {
			if _, e = js.AddStream(s); e != nil {
				return e
			}
		}
	}
	return nil
}
func (a *App) PublishOutbox(ctx context.Context, js nats.JetStreamContext) error {
	rows, e := queryJSON(ctx, a.DB.Pool, "SELECT to_jsonb(o) FROM outbox o WHERE published_at IS NULL ORDER BY created_at LIMIT 100")
	if e != nil {
		return e
	}
	for _, o := range rows {
		m := nats.NewMsg(str(o, "subject"))
		m.Data = encode(o["payload"])
		m.Header.Set(nats.MsgIdHdr, str(o, "id"))
		m.Header.Set("traceparent", str(sub(o, "payload"), "traceparent"))
		if _, e = js.PublishMsg(m, nats.Context(ctx)); e != nil {
			_, _ = a.DB.Pool.Exec(ctx, "UPDATE outbox SET attempts=attempts+1 WHERE id=$1", str(o, "id"))
			return e
		}
		if _, e = a.DB.Pool.Exec(ctx, "UPDATE outbox SET published_at=now(),attempts=attempts+1 WHERE id=$1", str(o, "id")); e != nil {
			return e
		}
	}
	return nil
}
func (a *App) Consume(ctx context.Context, js nats.JetStreamContext) error {
	sub, e := js.PullSubscribe("poligon.evt.>", "platform-results-v1", nats.BindStream("POLIGON_EVENTS"), nats.ManualAck(), nats.AckExplicit(), nats.AckWait(30*time.Second), nats.MaxDeliver(8))
	if e != nil {
		return e
	}
	defer sub.Unsubscribe()
	for ctx.Err() == nil {
		messages, e := sub.Fetch(16, nats.MaxWait(time.Second))
		if e != nil {
			if errors.Is(e, nats.ErrTimeout) {
				continue
			}
			return e
		}
		for _, m := range messages {
			work, cancel := context.WithTimeout(ctx, 20*time.Second)
			e = a.ApplyEvent(work, m.Subject, m.Data)
			cancel()
			if e == nil {
				_ = m.Ack()
				continue
			}
			var api *APIError
			permanent := errors.As(e, &api) && api.Status < 500
			meta, _ := m.Metadata()
			if permanent || meta != nil && meta.NumDelivered >= 8 {
				_, qe := a.DB.Pool.Exec(ctx, "INSERT INTO quarantine(id,subject,reason,payload_sha256) VALUES($1,$2,$3,$4)", ID(), m.Subject, fmt.Sprintf("%T", e), hash(m.Data))
				if qe == nil {
					_ = m.Term()
					slog.Error("event quarantined", "subject", m.Subject)
				} else {
					_ = m.NakWithDelay(5 * time.Second)
				}
			} else {
				_ = m.NakWithDelay(5 * time.Second)
			}
		}
	}
	return ctx.Err()
}
