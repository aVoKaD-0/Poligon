package platform

import (
	"context"
	"encoding/json"
	"errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

const runJSON = `to_jsonb(r) - 'request' - 'event_cursor' || jsonb_build_object('revision',r.revision::text,'last_event_cursor',r.event_cursor::text,'agent_version_id',r.request->'agent_version_id','evaluator_version_id',r.request->'evaluator_version_id','scenario_version_ids',r.request->'scenario_version_ids','seed',r.request->'seed','repeats',r.request->'repeats','session_count',(SELECT count(*) FROM sessions WHERE run_id=r.id),'counts_by_status',coalesce((SELECT jsonb_object_agg(status,n) FROM (SELECT status,count(*) n FROM sessions WHERE run_id=r.id GROUP BY status) x),'{}'),'counts_by_verdict',coalesce((SELECT jsonb_object_agg(verdict,n) FROM (SELECT coalesce(verdict,'pending') verdict,count(*) n FROM sessions WHERE run_id=r.id GROUP BY verdict) x),'{}'))`

func getRun(ctx context.Context, q Queryer, p, id string) (Object, error) {
	if !validID(id) {
		return nil, notFound()
	}
	return oneJSON(ctx, q, "SELECT "+runJSON+" FROM runs r WHERE project_id=$1 AND id=$2", p, id)
}
func getSession(ctx context.Context, q Queryer, p, id string) (Object, error) {
	if !validID(id) {
		return nil, notFound()
	}
	return oneJSON(ctx, q, "SELECT to_jsonb(s) FROM sessions s WHERE project_id=$1 AND id=$2", p, id)
}
func emit(ctx context.Context, tx pgx.Tx, runID, event string, payload any) error {
	var cursor int64
	if e := tx.QueryRow(ctx, "UPDATE runs SET event_cursor=event_cursor+1,revision=revision+1 WHERE id=$1 RETURNING event_cursor", runID).Scan(&cursor); e != nil {
		return e
	}
	_, e := tx.Exec(ctx, "INSERT INTO run_events(run_id,cursor,type,payload) VALUES($1,$2,$3,$4)", runID, cursor, event, encode(payload))
	return e
}
func aggregate(ctx context.Context, tx pgx.Tx, runID string) error {
	var p, status string
	var cancel bool
	e := tx.QueryRow(ctx, "SELECT project_id::text,status,cancel_requested FROM runs WHERE id=$1", runID).Scan(&p, &status, &cancel)
	if e != nil {
		return e
	}
	var active, completed, failed, uncertain int
	e = tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status NOT IN ('completed','failed','cancelled')),count(*) FILTER(WHERE status='completed'),count(*) FILTER(WHERE verdict='fail'),count(*) FILTER(WHERE status<>'completed' OR verdict='inconclusive' OR verdict IS NULL) FROM sessions WHERE run_id=$1`, runID).Scan(&active, &completed, &failed, &uncertain)
	if e != nil {
		return e
	}
	var verdict any
	next := status
	if cancel {
		next = "cancelling"
		if active == 0 {
			next = "cancelled"
		}
	} else if active == 0 {
		if completed == 0 {
			next = "failed"
		} else {
			next = "completed"
			verdict = "pass"
			if uncertain > 0 {
				verdict = "inconclusive"
			}
			if failed > 0 {
				verdict = "fail"
			}
		}
	} else if status == "queued" {
		var started bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM sessions WHERE run_id=$1 AND status<>'queued')", runID).Scan(&started); e != nil {
			return e
		}
		if started {
			next = "running"
		}
	}
	_, e = tx.Exec(ctx, "UPDATE runs SET status=$2,verdict=$3,started_at=CASE WHEN $2<>'queued' THEN coalesce(started_at,now()) ELSE started_at END,finished_at=CASE WHEN $4 THEN coalesce(finished_at,now()) ELSE NULL END WHERE id=$1", runID, next, verdict, terminal(next))
	if e != nil {
		return e
	}
	v, e := getRun(ctx, tx, p, runID)
	if e != nil {
		return e
	}
	v["revision"] = strconv.FormatInt(revision(v)+1, 10)
	var cursor int64
	_ = tx.QueryRow(ctx, "SELECT event_cursor FROM runs WHERE id=$1", runID).Scan(&cursor)
	v["last_event_cursor"] = strconv.FormatInt(cursor+1, 10)
	return emit(ctx, tx, runID, "run.updated", v)
}
func enqueue(ctx context.Context, tx pgx.Tx, typ, p, run, s string, attempt int, payload Object) error {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	var causation any
	if id, ok := ctx.Value(ctxKey("causation_id")).(string); ok {
		causation = id
	}
	id := ID()
	subject := "poligon.cmd." + typ + ".v1"
	env := Object{"schema_version": 1, "message_id": id, "producer_event_id": id, "type": typ + ".v1", "producer": "platform", "occurred_at": utc(), "project_id": p, "run_id": run, "session_id": s, "attempt_no": attempt, "correlation_id": run, "causation_id": causation, "traceparent": carrier.Get("traceparent"), "payload": payload}
	_, e := tx.Exec(ctx, "INSERT INTO outbox(id,subject,payload) VALUES($1,$2,$3)", id, subject, encode(env))
	return e
}
func addAttempt(ctx context.Context, tx pgx.Tx, p, run, s string, n int, plan Object) error {
	plan["context"] = Object{"project_id": p, "run_id": run, "session_id": s, "attempt_no": n}
	b, h, e := canonical(plan)
	if e != nil {
		return e
	}
	dispatch, ref := ID(), ID()
	_, e = tx.Exec(ctx, "INSERT INTO execution_attempts(session_id,attempt_no,dispatch_id,plan_ref,plan,plan_sha256) VALUES($1,$2,$3,$4,$5,$6)", s, n, dispatch, ref, b, h)
	if e != nil {
		return e
	}
	return enqueue(ctx, tx, "session.execute", p, run, s, n, Object{"dispatch_id": dispatch, "runtime_plan_ref": ref, "runtime_plan_sha256": h})
}
func addEvaluation(ctx context.Context, tx pgx.Tx, s Object, evaluator, purpose, group string, index int) (string, error) {
	id := ID()
	if group == "" {
		group = ID()
	}
	sid := str(s, "id")
	attempt := integer(s, "current_attempt_no", 1)
	var ref []byte
	e := tx.QueryRow(ctx, "SELECT manifest_ref FROM execution_attempts WHERE session_id=$1 AND attempt_no=$2 AND status='executed'", sid, attempt).Scan(&ref)
	if e != nil {
		return "", conflict("Нет исполненного manifest")
	}
	_, e = tx.Exec(ctx, "INSERT INTO evaluation_jobs(id,session_id,attempt_no,evaluation_group_id,purpose,retry_index,evaluator_version_id) VALUES($1,$2,$3,$4,$5,$6,$7)", id, sid, attempt, group, purpose, index, evaluator)
	if e != nil {
		return "", e
	}
	if purpose == "primary" {
		_, e = tx.Exec(ctx, "UPDATE sessions SET status='evaluating',current_evaluation_id=$2 WHERE id=$1", sid, id)
		if e != nil {
			return "", e
		}
	}
	manifest := obj(ref)
	e = enqueue(ctx, tx, "evaluation.execute", str(s, "project_id"), str(s, "run_id"), sid, attempt, Object{"evaluation_id": id, "manifest_ref": manifest, "manifest_sha256": str(manifest, "sha256"), "evaluator_version_id": evaluator})
	return id, e
}
func (a *App) idempotent(ctx context.Context, tx pgx.Tx, r *http.Request, o Object, fn func() (Object, error)) (Object, error) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 200 {
		return nil, invalid("Требуется Idempotency-Key длиной 1–200")
	}
	_, h, e := canonical(o)
	if e != nil {
		return nil, invalid("Некорректный JSON")
	}
	var oldHash string
	var response []byte
	e = tx.QueryRow(ctx, "SELECT body_hash,response FROM http_idempotency WHERE actor=$1 AND route=$2 AND key=$3 AND expires_at>now()", actor(ctx).ID, r.URL.Path, key).Scan(&oldHash, &response)
	if e == nil {
		if oldHash != h {
			return nil, conflict("Idempotency-Key уже использован с другим телом")
		}
		return obj(response), nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return nil, e
	}
	v, e := fn()
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, "INSERT INTO http_idempotency(actor,route,key,body_hash,response,status,expires_at) VALUES($1,$2,$3,$4,$5,202,now()+interval '24 hours') ON CONFLICT(actor,route,key) DO UPDATE SET body_hash=excluded.body_hash,response=excluded.response,status=202,expires_at=excluded.expires_at", actor(ctx).ID, r.URL.Path, key, h, encode(v))
	return v, e
}
func (a *App) replay(ctx context.Context, r *http.Request, o Object) (Object, error) {
	var result Object
	e := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		var b []byte
		var h string
		e := tx.QueryRow(ctx, "SELECT body_hash,response FROM http_idempotency WHERE actor=$1 AND route=$2 AND key=$3 AND expires_at>now()", actor(ctx).ID, r.URL.Path, r.Header.Get("Idempotency-Key")).Scan(&h, &b)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		_, actual, e := canonical(o)
		if e != nil {
			return e
		}
		if actual != h {
			return conflict("Idempotency-Key уже использован с другим телом")
		}
		result = obj(b)
		return nil
	})
	return result, e
}
func (a *App) prepareRun(ctx context.Context, p string, o Object) ([]Object, error) {
	if e := requireFields(o, []string{"agent_version_id", "scenario_version_ids", "evaluator_version_id", "repeats", "seed", "concurrency"}, "agent_version_id", "scenario_version_ids", "evaluator_version_id", "repeats", "seed", "concurrency"); e != nil {
		return nil, e
	}
	ids := stringsOf(o["scenario_version_ids"])
	repeat, concurrency := integer(o, "repeats", 0), integer(o, "concurrency", 0)
	if len(ids) < 1 || len(ids) > 100 || repeat < 1 || repeat > 10 || len(ids)*repeat > 200 || concurrency < 1 || concurrency > 20 {
		return nil, invalid("Превышены ограничения сценариев, повторов или concurrency")
	}
	if seed, e := strconv.ParseInt(stringNumber(o["seed"]), 10, 64); e != nil || seed < -9007199254740991 || seed > 9007199254740991 {
		return nil, invalid("seed должен быть целым числом в диапазоне безопасных JSON integers")
	}
	agent, e := a.published(ctx, p, str(o, "agent_version_id"), "agent")
	if e != nil {
		return nil, e
	}
	evaluator, e := a.published(ctx, p, str(o, "evaluator_version_id"), "evaluator")
	if e != nil {
		return nil, e
	}
	ad := sub(agent, "definition")
	if e = a.Config.ValidateEndpoint(ctx, str(ad, "endpoint")); e != nil {
		return nil, e
	}
	if ref := str(ad, "credential_ref"); ref != "" {
		if e = a.credentialValid(ctx, p, ref); e != nil {
			return nil, e
		}
	}
	plans := []Object{}
	scenarios := []Object{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, invalid("Повторяющийся scenario_version_id")
		}
		seen[id] = true
		s, e := a.published(ctx, p, id, "scenario")
		if e != nil {
			return nil, e
		}
		d := sub(s, "definition")
		modality := str(d, "modality")
		if modality == "text" && str(ad, "adapter") != "http-json-v1" || modality == "voice" && str(ad, "adapter") != "ws-pcm-v1" {
			return nil, invalid("Несовместимая modality агента")
		}
		env, e := a.published(ctx, p, str(d, "environment_version_id"), "environment")
		if e != nil {
			return nil, e
		}
		sim, e := a.published(ctx, p, str(d, "simulator_version_id"), "simulator")
		if e != nil {
			return nil, e
		}
		scenarios = append(scenarios, s)
		plans = append(plans, Object{"schema_version": 1, "versions": Object{"agent_version_id": agent["id"], "scenario_version_id": s["id"], "evaluator_version_id": evaluator["id"], "environment_version_id": env["id"], "simulator_version_id": sim["id"]}, "version_hashes": Object{"agent": agent["definition_sha256"], "scenario": s["definition_sha256"], "environment": env["definition_sha256"], "simulator": sim["definition_sha256"], "evaluator": evaluator["definition_sha256"]}, "agent": ad, "scenario": d, "environment": env["definition"], "simulator": sim["definition"], "evaluator": evaluator["definition"], "agent_adapter": ad["adapter"], "credential_revision_ref": ad["credential_ref"], "limits": d["limits"]})
	}
	if a.Validators == nil {
		return nil, fail(503, "DEPENDENCY_UNAVAILABLE", "Validators не настроены")
	}
	if e = a.Validators.CheckCompatibility(ctx, agent, scenarios, evaluator); e != nil {
		return nil, e
	}
	return plans, nil
}
func stringNumber(v any) string {
	switch x := v.(type) {
	case json.Number:
		return string(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return ""
}
func (a *App) createRun(ctx context.Context, tx pgx.Tx, r *http.Request, o Object, plans []Object) (Object, error) {
	p := r.PathValue("project_id")
	var quota int
	if e := tx.QueryRow(ctx, "SELECT concurrency FROM projects WHERE id=$1", p).Scan(&quota); e != nil {
		return nil, e
	}
	if integer(o, "concurrency", 1) > quota {
		return nil, fail(429, "QUOTA_EXCEEDED", "Concurrency превышает квоту проекта")
	}
	if ref := str(sub(plans[0], "agent"), "credential_ref"); ref != "" {
		var ok bool
		if e := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM credentials WHERE id=$1 AND project_id=$2 AND revoked_at IS NULL)", ref, p).Scan(&ok); e != nil {
			return nil, e
		}
		if !ok {
			return nil, invalid("Credential отозван")
		}
	}
	id := ID()
	created := utc()
	_, e := tx.Exec(ctx, "INSERT INTO runs(id,project_id,status,request,concurrency,created_at) VALUES($1,$2,'queued',$3,$4,$5)", id, p, encode(o), integer(o, "concurrency", 1), created)
	if e != nil {
		return nil, e
	}
	repeats := integer(o, "repeats", 1)
	seed, _ := strconv.ParseInt(stringNumber(o["seed"]), 10, 64)
	for _, template := range plans {
		for i := 0; i < repeats; i++ {
			plan := obj(encode(template))
			sid := ID()
			d := sub(plan, "scenario")
			_, e = tx.Exec(ctx, "INSERT INTO sessions(id,project_id,run_id,scenario_version_id,dataset_item_id,repetition_index,seed) VALUES($1,$2,$3,$4,$5,$6,$7)", sid, p, id, sub(plan, "versions")["scenario_version_id"], d["dataset_item_id"], i, seed)
			if e != nil {
				return nil, e
			}
			plan["seed"] = seed
			if e = addAttempt(ctx, tx, p, id, sid, 1, plan); e != nil {
				return nil, e
			}
		}
	}
	if e = audit(ctx, tx, p, actor(ctx).ID, "run.created", id); e != nil {
		return nil, e
	}
	if e = aggregate(ctx, tx, id); e != nil {
		return nil, e
	}
	return Object{"run_id": id, "status": "queued", "session_count": len(plans) * repeats, "created_at": created}, nil
}
func (a *App) runs(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := method(r, "GET", "POST"); e != nil {
		return nil, e
	}
	role := ""
	if r.Method == "POST" {
		role = "editor"
	}
	if e := a.authorize(r, role); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p := r.PathValue("project_id")
	if r.Method == "GET" {
		n, c, e := page(r)
		if e != nil {
			return nil, e
		}
		v, e := queryJSON(ctx, a.DB.Pool, "SELECT "+runJSON+" FROM runs r WHERE project_id=$1 AND id>$2 ORDER BY id LIMIT $3", p, c, n+1)
		return pageResult(v, n), e
	}
	o, e := body(w, r)
	if e != nil {
		return nil, e
	}
	if v, e := a.replay(ctx, r, o); v != nil || e != nil {
		if e == nil {
			writeJSON(w, 202, v)
		}
		return nil, e
	}
	plans, e := a.prepareRun(ctx, p, o)
	if e != nil {
		return nil, e
	}
	var v Object
	e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		v, err = a.idempotent(ctx, tx, r, o, func() (Object, error) { return a.createRun(ctx, tx, r, o, plans) })
		return err
	})
	if e == nil {
		writeJSON(w, 202, v)
	}
	return nil, e
}
func (a *App) run(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, ""); e != nil {
		return nil, e
	}
	v, e := getRun(r.Context(), a.DB.Pool, r.PathValue("project_id"), r.PathValue("run_id"))
	etag(w, v)
	return v, e
}
func (a *App) session(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, ""); e != nil {
		return nil, e
	}
	v, e := getSession(r.Context(), a.DB.Pool, r.PathValue("project_id"), r.PathValue("session_id"))
	if e != nil {
		return nil, e
	}
	attempts, e := queryJSON(r.Context(), a.DB.Pool, "SELECT to_jsonb(t)-'plan'-'worker_id'-'lease_id' FROM execution_attempts t WHERE session_id=$1 ORDER BY attempt_no", r.PathValue("session_id"))
	v["attempts"] = attempts
	return v, e
}
func (a *App) sessionList(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, ""); e != nil {
		return nil, e
	}
	if _, e := getRun(r.Context(), a.DB.Pool, r.PathValue("project_id"), r.PathValue("run_id")); e != nil {
		return nil, e
	}
	n, c, e := page(r)
	if e != nil {
		return nil, e
	}
	v, e := queryJSON(r.Context(), a.DB.Pool, "SELECT to_jsonb(s) FROM sessions s WHERE run_id=$1 AND id>$2 ORDER BY id LIMIT $3", r.PathValue("run_id"), c, n+1)
	return pageResult(v, n), e
}
func (a *App) cancelRun(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, "editor"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p, id := r.PathValue("project_id"), r.PathValue("run_id")
	e := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		v, e := getRun(ctx, tx, p, id)
		if e != nil {
			return e
		}
		if terminal(str(v, "status")) {
			return nil
		}
		if _, e = tx.Exec(ctx, "UPDATE runs SET cancel_requested=true,status='cancelling' WHERE id=$1", id); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE execution_attempts SET status='cancelled' WHERE session_id IN(SELECT id FROM sessions WHERE run_id=$1) AND status='queued'", id); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE evaluation_jobs SET cancel_requested=true,status=CASE WHEN status='queued' THEN 'cancelled' ELSE status END WHERE session_id IN(SELECT id FROM sessions WHERE run_id=$1) AND status IN('queued','running')", id); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE sessions s SET status='cancelled',verdict=NULL WHERE run_id=$1 AND status NOT IN('completed','failed','cancelled') AND NOT EXISTS(SELECT 1 FROM execution_attempts a WHERE a.session_id=s.id AND a.attempt_no=s.current_attempt_no AND a.status IN('leased','preparing','running')) AND NOT EXISTS(SELECT 1 FROM evaluation_jobs e WHERE e.session_id=s.id AND e.status='running')`, id); e != nil {
			return e
		}
		if e = audit(ctx, tx, p, actor(ctx).ID, "run.cancel_requested", id); e != nil {
			return e
		}
		return aggregate(ctx, tx, id)
	})
	if e != nil {
		return nil, e
	}
	v, e := getRun(ctx, a.DB.Pool, p, id)
	if e == nil {
		writeJSON(w, 202, v)
	}
	return nil, e
}
func (a *App) retryRun(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, "editor"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p, id := r.PathValue("project_id"), r.PathValue("run_id")
	o, e := body(w, r)
	if e != nil {
		return nil, e
	}
	if len(o) != 0 {
		return nil, invalid("retry принимает пустой объект")
	}
	if v, e := a.replay(ctx, r, o); v != nil || e != nil {
		if e == nil {
			writeJSON(w, 202, v)
		}
		return nil, e
	}
	v, e := getRun(ctx, a.DB.Pool, p, id)
	if e != nil {
		return nil, e
	}
	if !terminal(str(v, "status")) {
		return nil, conflict("Исходный run ещё активен")
	}
	var b []byte
	if e = a.DB.Pool.QueryRow(ctx, "SELECT request FROM runs WHERE id=$1", id).Scan(&b); e != nil {
		return nil, e
	}
	req := obj(b)
	plans, e := a.prepareRun(ctx, p, req)
	if e != nil {
		return nil, e
	}
	e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		v, err = a.idempotent(ctx, tx, r, o, func() (Object, error) { return a.createRun(ctx, tx, r, req, plans) })
		return err
	})
	if e == nil {
		writeJSON(w, 202, v)
	}
	return nil, e
}
func (a *App) reevaluate(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, "editor"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p, sid := r.PathValue("project_id"), r.PathValue("session_id")
	o, e := body(w, r)
	if e != nil {
		return nil, e
	}
	if e = requireFields(o, []string{"evaluator_version_id"}, "evaluator_version_id"); e != nil {
		return nil, e
	}
	if v, e := a.replay(ctx, r, o); v != nil || e != nil {
		if e == nil {
			writeJSON(w, 202, v)
		}
		return nil, e
	}
	evaluator, e := a.published(ctx, p, str(o, "evaluator_version_id"), "evaluator")
	if e != nil {
		return nil, e
	}
	source, e := getSession(ctx, a.DB.Pool, p, sid)
	if e != nil {
		return nil, e
	}
	var original []byte
	if e = a.DB.Pool.QueryRow(ctx, "SELECT plan FROM execution_attempts WHERE session_id=$1 AND attempt_no=$2", sid, integer(source, "current_attempt_no", 1)).Scan(&original); e != nil {
		return nil, e
	}
	refs := sub(obj(original), "versions")
	agent, e := a.published(ctx, p, str(refs, "agent_version_id"), "agent")
	if e != nil {
		return nil, e
	}
	scenario, e := a.published(ctx, p, str(refs, "scenario_version_id"), "scenario")
	if e != nil {
		return nil, e
	}
	if a.Validators == nil {
		return nil, fail(503, "DEPENDENCY_UNAVAILABLE", "Validators unavailable")
	}
	if e = a.Validators.CheckCompatibility(ctx, agent, []Object{scenario}, evaluator); e != nil {
		return nil, e
	}
	var v Object
	e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		v, err = a.idempotent(ctx, tx, r, o, func() (Object, error) {
			s, e := getSession(ctx, tx, p, sid)
			if e != nil {
				return nil, e
			}
			if !terminal(str(s, "status")) {
				return nil, conflict("Сессия ещё активна")
			}
			var purging bool
			if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM artifact_index WHERE session_id=$1 AND attempt_no=$2 AND delete_pending)", sid, integer(s, "current_attempt_no", 1)).Scan(&purging); e != nil {
				return nil, e
			}
			if purging || str(sub(s, "completeness"), "status") == "expired" {
				return nil, conflict("Evidence retention expired")
			}
			var active bool
			if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM evaluation_jobs WHERE session_id=$1 AND purpose='manual' AND current AND status IN('queued','running'))", sid).Scan(&active); e != nil {
				return nil, e
			}
			if active {
				return nil, conflict("Дополнительная оценка уже выполняется")
			}
			group := ID()
			id, e := addEvaluation(ctx, tx, s, str(o, "evaluator_version_id"), "manual", group, 1)
			if e != nil {
				return nil, e
			}
			if e = audit(ctx, tx, p, actor(ctx).ID, "evaluation.manual", id); e != nil {
				return nil, e
			}
			return Object{"evaluation_id": id, "evaluation_group_id": group, "purpose": "manual", "status": "queued"}, nil
		})
		return err
	})
	if e == nil {
		writeJSON(w, 202, v)
	}
	return nil, e
}
func (a *App) evaluations(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, ""); e != nil {
		return nil, e
	}
	ctx := r.Context()
	sid := r.PathValue("session_id")
	if _, e := getSession(ctx, a.DB.Pool, r.PathValue("project_id"), sid); e != nil {
		return nil, e
	}
	if id := r.PathValue("evaluation_id"); id != "" {
		if !validID(id) {
			return nil, notFound()
		}
		return oneJSON(ctx, a.DB.Pool, "SELECT to_jsonb(e)-'worker_id'-'lease_id' FROM evaluation_jobs e WHERE session_id=$1 AND id=$2", sid, id)
	}
	n, c, e := page(r)
	if e != nil {
		return nil, e
	}
	v, e := queryJSON(ctx, a.DB.Pool, "SELECT to_jsonb(e)-'worker_id'-'lease_id' FROM evaluation_jobs e WHERE session_id=$1 AND id>$2 ORDER BY id LIMIT $3", sid, c, n+1)
	return pageResult(v, n), e
}
func (a *App) assessment(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, ""); e != nil {
		return nil, e
	}
	s, e := getSession(r.Context(), a.DB.Pool, r.PathValue("project_id"), r.PathValue("session_id"))
	if e != nil {
		return nil, e
	}
	if s["primary_evaluation_id"] == nil {
		return Object{"assessment": nil, "status": s["status"]}, nil
	}
	return oneJSON(r.Context(), a.DB.Pool, "SELECT jsonb_build_object('evaluation_id',id,'purpose',purpose,'assessment',assessment,'assessment_ref',assessment_ref) FROM evaluation_jobs WHERE id=$1", s["primary_evaluation_id"])
}
func (a *App) cancelEvaluation(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, "editor"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	sid, id := r.PathValue("session_id"), r.PathValue("evaluation_id")
	if !validID(id) {
		return nil, notFound()
	}
	e := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		if _, e := getSession(ctx, tx, r.PathValue("project_id"), sid); e != nil {
			return e
		}
		v, e := oneJSON(ctx, tx, "SELECT to_jsonb(e) FROM evaluation_jobs e WHERE session_id=$1 AND id=$2", sid, id)
		if e != nil {
			return e
		}
		if str(v, "purpose") != "manual" {
			return conflict("Основная оценка отменяется вместе с run")
		}
		_, e = tx.Exec(ctx, "UPDATE evaluation_jobs SET cancel_requested=true,status=CASE WHEN status='queued' THEN 'cancelled' ELSE status END WHERE id=$1 AND status IN('queued','running')", id)
		return e
	})
	if e == nil {
		writeJSON(w, 202, Object{"evaluation_id": id, "cancel_requested": true})
	}
	return nil, e
}
