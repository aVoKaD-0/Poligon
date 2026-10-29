package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	pb "github.com/poligon/platform/gen/controlv1"
	"github.com/poligon/platform/migrations"
	"google.golang.org/protobuf/proto"
)

type fakeValidators struct{ err error }

func (f fakeValidators) ValidateScenario(context.Context, Object) error { return f.err }
func (f fakeValidators) CheckCompatibility(context.Context, Object, []Object, Object) error {
	return f.err
}

type fixture struct {
	a                        *App
	http                     http.Handler
	cookies                  []*http.Cookie
	csrf, p, agent, scenario string
}

func integrationDB(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; integration test requires real PostgreSQL")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	name := "test_" + strings.ReplaceAll(ID(), "-", "")
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	config, e := pgxpool.ParseConfig(url)
	if e != nil {
		t.Fatal(e)
	}
	config.ConnConfig.Database = name
	pool, e := pgxpool.NewWithConfig(ctx, config)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	if e = migrations.Apply(ctx, pool); e != nil {
		t.Fatal(e)
	}
	if e = migrations.Apply(ctx, pool); e != nil {
		t.Fatal("idempotent migrations", e)
	}
	return &DB{pool}
}
func (f *fixture) request(method, path string, o any, headers map[string]string) *httptest.ResponseRecorder {
	var b []byte
	if o != nil {
		b = encode(o)
	}
	req := httptest.NewRequest(method, "http://platform.test"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://platform.test")
	req.Header.Set("X-CSRF-Token", f.csrf)
	for _, c := range f.cookies {
		req.AddCookie(c)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.http.ServeHTTP(w, req)
	return w
}
func requireStatus(t *testing.T, w *httptest.ResponseRecorder, want int) Object {
	t.Helper()
	if w.Code != want {
		t.Fatalf("HTTP %d (wanted %d): %s", w.Code, want, w.Body.String())
	}
	return obj(w.Body.Bytes())
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	a := &App{DB: integrationDB(t), Config: Config{Env: "local", Origin: "http://platform.test", MasterKey: make([]byte, 32), AllowedHosts: []string{"localhost"}, AllowPrivate: true, GlobalConcurrency: 50, GlobalEvaluations: 20}, Validators: fakeValidators{}}
	ctx := context.Background()
	if e := a.SeedCatalog(ctx); e != nil {
		t.Fatal(e)
	}
	f := &fixture{a: a, http: a.Handler()}
	login := f.request("GET", "/api/v1/auth/login", nil, nil)
	if login.Code != 302 {
		t.Fatal(login.Body.String())
	}
	f.cookies = login.Result().Cookies()
	profile := requireStatus(t, f.request("GET", "/api/v1/me", nil, nil), 200)
	f.csrf = str(profile, "csrf_token")
	f.p = str(requireStatus(t, f.request("POST", "/api/v1/projects", Object{"name": "Test"}, nil), 201), "id")
	base := "/api/v1/projects/" + f.p
	agent := requireStatus(t, f.request("POST", base+"/agents", Object{"title": "Agent"}, nil), 201)
	v := requireStatus(t, f.request("POST", base+"/agents/"+str(agent, "id")+"/versions", Object{"definition": Object{"schema_version": 1, "adapter": "http-json-v1", "endpoint": "http://localhost:9001"}}, nil), 201)
	path := base + "/agents/" + str(agent, "id") + "/versions/" + str(v, "id")
	v = requireStatus(t, f.request("POST", path+"/publish", nil, map[string]string{"If-Match": `"1"`}), 200)
	f.agent = str(v, "id")
	scenario := requireStatus(t, f.request("POST", base+"/scenarios", Object{"title": "Case"}, nil), 201)
	definition := Object{"schema_version": 1, "dataset_item_id": scenario["dataset_item_id"], "title": "Timeout after commit", "modality": "text", "environment_version_id": "a0000000-0000-4000-8000-000000000001", "template_id": "field-service", "template_version": "v1", "initial_fixture": Object{}, "actor_customer_ref": "customer-a", "virtual_clock": "2026-01-12T08:00:00Z", "business_timezone": "Europe/Moscow", "client_goal": "Создать ровно одну заявку", "client_known_facts": Object{}, "simulator_version_id": "a0000000-0000-4000-8000-000000000002", "fault_rules": []any{}, "assertions": []any{Object{"rule_id": "booking.count.v1", "required": true}}, "limits": Object{"max_client_turns": 20, "max_tool_calls": 50, "session_duration_ms": 300000}, "seed_policy": Object{"mode": "fixed"}}
	v = requireStatus(t, f.request("POST", base+"/scenarios/"+str(scenario, "id")+"/versions", Object{"definition": definition}, nil), 201)
	path = base + "/scenarios/" + str(scenario, "id") + "/versions/" + str(v, "id")
	v = requireStatus(t, f.request("POST", path+"/publish", nil, map[string]string{"If-Match": `"1"`}), 200)
	f.scenario = str(v, "id")
	return f
}
func (f *fixture) runRequest() Object {
	return Object{"agent_version_id": f.agent, "scenario_version_ids": []any{f.scenario}, "evaluator_version_id": "a0000000-0000-4000-8000-000000000003", "repeats": 1, "seed": 42, "concurrency": 2}
}
func (f *fixture) start(t *testing.T) Object {
	return requireStatus(t, f.request("POST", "/api/v1/projects/"+f.p+"/runs", f.runRequest(), map[string]string{"Idempotency-Key": ID()}), 202)
}
func TestConcurrentRunIdempotencyAndOutbox(t *testing.T) {
	f := newFixture(t)
	path := "/api/v1/projects/" + f.p + "/runs"
	key := ID()
	const count = 8
	responses := make(chan *httptest.ResponseRecorder, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses <- f.request("POST", path, f.runRequest(), map[string]string{"Idempotency-Key": key})
		}()
	}
	wg.Wait()
	close(responses)
	id := ""
	for w := range responses {
		v := requireStatus(t, w, 202)
		if id == "" {
			id = str(v, "run_id")
		}
		if str(v, "run_id") != id {
			t.Fatal("duplicate run")
		}
	}
	var runs, attempts, outbox int
	e := f.a.DB.Pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM runs),(SELECT count(*) FROM execution_attempts),(SELECT count(*) FROM outbox)").Scan(&runs, &attempts, &outbox)
	if e != nil || runs != 1 || attempts != 1 || outbox != 1 {
		t.Fatalf("non-atomic creation: %d %d %d %v", runs, attempts, outbox, e)
	}
	changed := f.runRequest()
	changed["seed"] = 43
	requireStatus(t, f.request("POST", path, changed, map[string]string{"Idempotency-Key": key}), 409)
	f.a.Validators = fakeValidators{fail(503, "UNAVAILABLE", "offline")}
	requireStatus(t, f.request("POST", path, f.runRequest(), map[string]string{"Idempotency-Key": key}), 202)
}
func TestLeaseRetryStaleEventAndCancellation(t *testing.T) {
	f := newFixture(t)
	run := f.start(t)
	ctx := context.WithValue(context.Background(), ctxKey("service"), "runner")
	var sid, dispatch string
	e := f.a.DB.Pool.QueryRow(ctx, "SELECT s.id::text,a.dispatch_id::text FROM sessions s JOIN execution_attempts a ON a.session_id=s.id WHERE s.run_id=$1", str(run, "run_id")).Scan(&sid, &dispatch)
	if e != nil {
		t.Fatal(e)
	}
	control := &Control{App: f.a}
	request := &pb.LeaseRequest{Context: &pb.ExecutionContext{ProjectId: f.p, RunId: str(run, "run_id"), SessionId: sid, AttemptNo: 1}, WorkerId: ID(), DispatchId: dispatch}
	lease, e := control.ClaimExecution(ctx, request)
	if e != nil || !lease.Accepted {
		t.Fatalf("claim: %v %v", lease, e)
	}
	again, e := control.ClaimExecution(ctx, request)
	if e != nil || again.LeaseId != lease.LeaseId {
		t.Fatal("claim replay must preserve lease")
	}
	different := proto.Clone(request).(*pb.LeaseRequest)
	different.WorkerId = ID()
	other, e := control.ClaimExecution(ctx, different)
	if e != nil || other.Accepted || other.Reason != "ALREADY_CLAIMED" {
		t.Fatal("two owners accepted")
	}
	_, e = f.a.DB.Pool.Exec(ctx, "UPDATE execution_attempts SET plan='{}' WHERE session_id=$1", sid)
	if e == nil {
		t.Fatal("immutable plan changed")
	}
	_, e = f.a.DB.Pool.Exec(ctx, "UPDATE execution_attempts SET lease_expiry=now()-interval '1 second' WHERE session_id=$1", sid)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.a.Supervise(ctx); e != nil {
		t.Fatal(e)
	}
	var attempt int
	if e = f.a.DB.Pool.QueryRow(ctx, "SELECT current_attempt_no FROM sessions WHERE id=$1", sid).Scan(&attempt); e != nil || attempt != 2 {
		t.Fatalf("retry: %d %v", attempt, e)
	}
	event := Envelope{SchemaVersion: 1, MessageID: ID(), ProducerEventID: ID(), Type: "session.progress.v1", Producer: "runner", OccurredAt: utc(), ProjectID: f.p, RunID: str(run, "run_id"), SessionID: sid, AttemptNo: 1, Payload: Object{"session_seq": 1, "event_kind": "agent.message"}}
	for i := 0; i < 2; i++ {
		if e = f.a.ApplyEvent(ctx, "poligon.evt.session.progress.v1", encode(event)); e != nil {
			t.Fatal(e)
		}
	}
	var stale int
	if e = f.a.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM stale_results").Scan(&stale); e != nil || stale != 1 {
		t.Fatal("stale result deduplication failed")
	}
	requireStatus(t, f.request("POST", "/api/v1/projects/"+f.p+"/runs/"+str(run, "run_id")+"/cancel", nil, nil), 202)
	view := requireStatus(t, f.request("GET", "/api/v1/projects/"+f.p+"/runs/"+str(run, "run_id"), nil, nil), 200)
	if str(view, "status") != "cancelled" || view["verdict"] != nil {
		t.Fatal(view)
	}
}
func TestTenantIsolationOwnerAndCSRF(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	viewer, foreign := ID(), ID()
	for _, id := range []string{viewer, foreign} {
		_, e := f.a.DB.Pool.Exec(ctx, "INSERT INTO users(id,issuer,subject,name) VALUES($1,'test',$2,'Other')", id, id)
		if e != nil {
			t.Fatal(e)
		}
	}
	_, e := f.a.DB.Pool.Exec(ctx, "INSERT INTO project_members(project_id,user_id,role) VALUES($1,$2,'viewer')", f.p, viewer)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		id   string
		want int
	}{{viewer, 200}, {foreign, 404}} {
		w := httptest.NewRecorder()
		if e = f.a.newSession(ctx, w, tc.id); e != nil {
			t.Fatal(e)
		}
		other := &fixture{a: f.a, http: f.http, cookies: w.Result().Cookies()}
		me := requireStatus(t, other.request("GET", "/api/v1/me", nil, nil), 200)
		other.csrf = str(me, "csrf_token")
		path := "/api/v1/projects/" + f.p
		requireStatus(t, other.request("GET", path, nil, nil), tc.want)
		want := 403
		if tc.want == 404 {
			want = 404
		}
		requireStatus(t, other.request("POST", path+"/agents", Object{"title": "Forbidden"}, nil), want)
	}
	me := requireStatus(t, f.request("GET", "/api/v1/me", nil, nil), 200)
	requireStatus(t, f.request("DELETE", "/api/v1/projects/"+f.p+"/members/"+str(me, "id"), nil, nil), 409)
	requireStatus(t, f.request("POST", "/api/v1/projects", Object{"name": "bad"}, map[string]string{"Origin": "https://evil.example"}), 403)
	requireStatus(t, f.request("POST", "/api/v1/projects", Object{"name": "bad"}, map[string]string{"X-CSRF-Token": "bad"}), 403)
}
func TestRealNATSOutboxRecovery(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}
	f := newFixture(t)
	run := f.start(t)
	nc, e := nats.Connect(url)
	if e != nil {
		t.Fatal(e)
	}
	defer nc.Close()
	js, e := nc.JetStream()
	if e != nil {
		t.Fatal(e)
	}
	if e = SetupStreams(js); e != nil {
		t.Fatal(e)
	}
	if e = f.a.PublishOutbox(context.Background(), js); e != nil {
		t.Fatal(e)
	}
	sub, e := js.PullSubscribe("poligon.cmd.session.execute.v1", "test-"+ID(), nats.BindStream("POLIGON_EXECUTION"))
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Unsubscribe()
	messages, e := sub.Fetch(1, nats.MaxWait(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	var command Object
	if e = json.Unmarshal(messages[0].Data, &command); e != nil {
		t.Fatal(e)
	}
	if str(command, "run_id") != str(run, "run_id") {
		t.Fatal("wrong command context")
	}
	_ = messages[0].Ack()
	if e = f.a.PublishOutbox(context.Background(), js); e != nil {
		t.Fatal(e)
	}
	var pending int
	_ = f.a.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM outbox WHERE published_at IS NULL").Scan(&pending)
	if pending != 0 {
		t.Fatal("outbox not drained")
	}
}
func TestPrimaryReportUnaffectedByManualEvaluation(t *testing.T) {
	f := newFixture(t)
	run := f.start(t)
	ctx := context.Background()
	var sid string
	_ = f.a.DB.Pool.QueryRow(ctx, "SELECT id::text FROM sessions WHERE run_id=$1", str(run, "run_id")).Scan(&sid)
	err := f.a.DB.Tx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, "UPDATE execution_attempts SET status='executed',manifest_ref=$2 WHERE session_id=$1", sid, encode(Object{"artifact_id": ID(), "sha256": strings.Repeat("a", 64)}))
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, "UPDATE sessions SET status='completed',verdict='pass' WHERE id=$1", sid)
		if e != nil {
			return e
		}
		return aggregate(ctx, tx, str(run, "run_id"))
	})
	if err != nil {
		t.Fatal(err)
	}
	before := requireStatus(t, f.request("GET", "/api/v1/projects/"+f.p+"/runs/"+str(run, "run_id"), nil, nil), 200)
	path := "/api/v1/projects/" + f.p + "/sessions/" + sid + "/reevaluate"
	manual := requireStatus(t, f.request("POST", path, Object{"evaluator_version_id": "a0000000-0000-4000-8000-000000000003"}, map[string]string{"Idempotency-Key": ID()}), 202)
	after := requireStatus(t, f.request("GET", "/api/v1/projects/"+f.p+"/runs/"+str(run, "run_id"), nil, nil), 200)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatal("manual evaluation changed primary run")
	}
	requireStatus(t, f.request("POST", "/api/v1/projects/"+f.p+"/sessions/"+sid+"/evaluations/"+str(manual, "evaluation_id")+"/cancel", nil, nil), 202)
}
