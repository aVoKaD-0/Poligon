package platform

import (
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"sort"
	"strconv"
	"time"
)

func (a *App) stream(w http.ResponseWriter, r *http.Request) {
	annotateHTTP(r)
	if e := a.authorize(r, ""); e != nil {
		writeError(w, r, e)
		return
	}
	p, id := r.PathValue("project_id"), r.PathValue("run_id")
	if _, e := getRun(r.Context(), a.DB.Pool, p, id); e != nil {
		writeError(w, r, e)
		return
	}
	a.metrics.sse.Add(1)
	defer a.metrics.sse.Add(-1)
	cursor := int64(0)
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after_cursor")
	}
	if raw != "" {
		n, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || n < 0 {
			writeError(w, r, invalid("Invalid event cursor"))
			return
		}
		cursor = n
	}
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, r, fail(500, "STREAM_UNAVAILABLE", "Streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	controller := http.NewResponseController(w)
	send := func(kind, id string, v any) error {
		_ = controller.SetWriteDeadline(utc().Add(5 * time.Second))
		if id != "" {
			if _, e := fmt.Fprintf(w, "id: %s\n", id); e != nil {
				return e
			}
		}
		if _, e := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, encode(v)); e != nil {
			return e
		}
		return controller.Flush()
	}
	defer controller.SetWriteDeadline(time.Time{})
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		if _, e := a.identity(r); e != nil {
			return
		}
		if e := a.authorize(r, ""); e != nil {
			return
		}
		var min, max int64
		e := a.DB.Pool.QueryRow(r.Context(), "SELECT coalesce((SELECT min(cursor) FROM run_events WHERE run_id=$1),0),event_cursor FROM runs WHERE id=$1", id).Scan(&min, &max)
		if e != nil {
			return
		}
		if cursor > max || min == 0 && cursor < max || min > cursor+1 {
			_ = send("stream.reset", "", Object{"run_id": id, "type": "stream.reset", "payload": Object{"reason": "CURSOR_OUTSIDE_RETENTION", "action": "reload_snapshot"}})
			return
		}
		events, e := queryJSON(r.Context(), a.DB.Pool, "SELECT jsonb_build_object('cursor',cursor::text,'run_id',run_id,'type',type,'payload',payload) FROM run_events WHERE run_id=$1 AND cursor>$2 ORDER BY cursor LIMIT 100", id, cursor)
		if e != nil {
			return
		}
		for _, v := range events {
			if e = send(str(v, "type"), str(v, "cursor"), v); e != nil {
				return
			}
			cursor, _ = strconv.ParseInt(str(v, "cursor"), 10, 64)
		}
		if len(events) == 100 {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
		case <-heartbeat.C:
			if send("heartbeat", "", Object{"server_time": utc()}) != nil {
				return
			}
		}
	}
}
func (a *App) sessionEvents(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, ""); e != nil {
		return nil, e
	}
	s, e := getSession(r.Context(), a.DB.Pool, r.PathValue("project_id"), r.PathValue("session_id"))
	if e != nil {
		return nil, e
	}
	attempt := integer(s, "current_attempt_no", 1)
	if raw := r.URL.Query().Get("attempt_no"); raw != "" {
		attempt, e = strconv.Atoi(raw)
		if e != nil || attempt < 1 {
			return nil, invalid("Invalid attempt_no")
		}
	}
	after := int64(0)
	if raw := r.URL.Query().Get("after_seq"); raw != "" {
		after, e = strconv.ParseInt(raw, 10, 64)
		if e != nil || after < 0 {
			return nil, invalid("Invalid after_seq")
		}
	}
	n, _, e := page(r)
	if e != nil {
		return nil, e
	}
	v, e := queryJSON(r.Context(), a.DB.Pool, "SELECT jsonb_build_object('session_seq',session_seq::text,'producer_event_id',producer_event_id,'payload',payload) FROM progress_events WHERE session_id=$1 AND attempt_no=$2 AND session_seq>$3 ORDER BY session_seq LIMIT $4", str(s, "id"), attempt, after, n+1)
	if e != nil {
		return nil, e
	}
	var next any
	if len(v) > n {
		v = v[:n]
		next = v[len(v)-1]["session_seq"]
	}
	return Object{"items": v, "next_cursor": next, "attempt_no": attempt, "completeness": Object{"status": str(sub(s, "completeness"), "status"), "evidence": s["completeness"]}}, nil
}
func (a *App) report(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, ""); e != nil {
		return nil, e
	}
	ctx := r.Context()
	tx, err := a.DB.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	p, id := r.PathValue("project_id"), r.PathValue("run_id")
	v, e := getRun(ctx, tx, p, id)
	if e != nil {
		return nil, e
	}
	sessions, e := queryJSON(ctx, tx, "SELECT to_jsonb(s)||jsonb_build_object('assessment',e.assessment,'assessment_ref',e.assessment_ref) FROM sessions s LEFT JOIN evaluation_jobs e ON e.id=s.primary_evaluation_id WHERE s.run_id=$1 ORDER BY s.dataset_item_id,s.repetition_index", id)
	if e != nil {
		return nil, e
	}
	counts := map[string]int{}
	pass, denominator, excluded, uncertain := 0, 0, 0, 0
	for _, s := range sessions {
		if str(s, "status") != "completed" {
			excluded++
			continue
		}
		if str(s, "verdict") == "inconclusive" {
			uncertain++
		} else {
			denominator++
			if str(s, "verdict") == "pass" {
				pass++
			}
		}
		assessment := sub(s, "assessment")
		assertions, _ := assessment["assertions"].([]any)
		for _, raw := range assertions {
			assertion, _ := raw.(map[string]any)
			required, _ := assertion["required"].(bool)
			prefix := "optional."
			if required {
				prefix = "required."
			}
			counts[prefix+str(assertion, "status")]++
		}
	}
	refs, e := queryJSON(ctx, tx, "SELECT jsonb_build_object('artifact_id',id,'kind',kind,'sha256',sha256,'size_bytes',size_bytes::text,'mime',mime,'session_id',session_id,'attempt_no',attempt_no) FROM artifact_index WHERE project_id=$1 AND run_id=$2 AND NOT delete_pending ORDER BY id", p, id)
	if e != nil {
		return nil, e
	}
	usage := Object{"input_tokens": 0, "output_tokens": 0, "audio_ms": 0, "execution_usage_status": "known", "provider_cost_status": "unknown"}
	manifests, err := queryJSON(ctx, tx, "SELECT jsonb_build_object('manifest',a.manifest) FROM sessions s JOIN execution_attempts a ON a.session_id=s.id AND a.attempt_no=s.current_attempt_no WHERE s.run_id=$1", id)
	if err != nil {
		return nil, err
	}
	for _, item := range manifests {
		m := sub(item, "manifest")
		u := sub(m, "usage")
		for _, field := range []string{"input_tokens", "output_tokens", "audio_ms"} {
			if u[field] == nil {
				usage["execution_usage_status"] = "unknown"
				continue
			}
			usage[field] = integer(usage, field, 0) + integer(u, field, 0)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return Object{"run": v, "sessions": sessions, "assertion_counts": counts, "pass_rate": Object{"passed": pass, "denominator": denominator, "excluded": excluded, "inconclusive": uncertain}, "usage": usage, "artifact_refs": refs}, nil
}
func comparable(a, b Object) []string {
	diff := []string{}
	for _, k := range []string{"evaluator_version_id", "scenario_version_ids", "seed", "repeats"} {
		av, bv := a[k], b[k]
		if k == "scenario_version_ids" {
			aa, bb := stringsOf(av), stringsOf(bv)
			sort.Strings(aa)
			sort.Strings(bb)
			av, bv = aa, bb
		}
		_, x, _ := canonical(av)
		_, y, _ := canonical(bv)
		if x != y {
			diff = append(diff, k)
		}
	}
	return diff
}
func (a *App) compare(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, ""); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p := r.PathValue("project_id")
	base, candidate := r.URL.Query().Get("baseline_run_id"), r.URL.Query().Get("candidate_run_id")
	b, e := getRun(ctx, a.DB.Pool, p, base)
	if e != nil {
		return nil, e
	}
	c, e := getRun(ctx, a.DB.Pool, p, candidate)
	if e != nil {
		return nil, e
	}
	diff := comparable(b, c)
	cases := func(id string) ([]Object, error) {
		return queryJSON(ctx, a.DB.Pool, "SELECT to_jsonb(s)||jsonb_build_object('environment_version_id',a.plan->'versions'->'environment_version_id','simulator_version_id',a.plan->'versions'->'simulator_version_id') FROM sessions s JOIN execution_attempts a ON a.session_id=s.id AND a.attempt_no=s.current_attempt_no WHERE s.run_id=$1", id)
	}
	bs, e := cases(base)
	if e != nil {
		return nil, e
	}
	cs, e := cases(candidate)
	if e != nil {
		return nil, e
	}
	key := func(s Object) string { return str(s, "dataset_item_id") + ":" + stringNumber(s["repetition_index"]) }
	bm, cm := map[string]Object{}, map[string]Object{}
	keys := map[string]bool{}
	for _, s := range bs {
		bm[key(s)] = s
		keys[key(s)] = true
	}
	for _, s := range cs {
		cm[key(s)] = s
		keys[key(s)] = true
	}
	for k := range keys {
		x, y := bm[k], cm[k]
		if x == nil || y == nil {
			continue
		}
		for _, field := range []string{"seed", "scenario_version_id", "environment_version_id", "simulator_version_id"} {
			if fmt.Sprint(x[field]) != fmt.Sprint(y[field]) {
				diff = append(diff, k+":"+field)
			}
		}
	}
	if len(diff) > 0 {
		return nil, &APIError{422, "INCOMPARABLE_RUNS", "Прогоны несовместимы", Object{"differences": diff}}
	}
	sorted := []string{}
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	rows := []Object{}
	for _, k := range sorted {
		x, y := bm[k], cm[k]
		change := "unchanged"
		if x == nil || y == nil {
			change = "missing"
		} else if str(x, "status") != "completed" || str(y, "status") != "completed" || str(x, "verdict") == "inconclusive" || str(y, "verdict") == "inconclusive" {
			change = "excluded"
		} else if str(x, "verdict") == "fail" && str(y, "verdict") == "pass" {
			change = "improved"
		} else if str(x, "verdict") == "pass" && str(y, "verdict") == "fail" {
			change = "regressed"
		}
		rows = append(rows, Object{"case": k, "baseline": x, "candidate": y, "change": change})
	}
	return Object{"baseline_run_id": base, "candidate_run_id": candidate, "items": rows}, nil
}
