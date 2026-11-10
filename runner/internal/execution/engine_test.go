package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/google/uuid"
	"github.com/poligon/runner/contracts"
	pb "github.com/poligon/runner/contracts/controlv1"
)

const fixtureID = "ab123456-ab12-4ab1-8ab1-123456789012"
const testSecret = "session-private-secret"

func planFor(t testing.TB) (Job, *pb.LeaseResponse) {
	t.Helper()
	j := Job{Scope: Scope{fixtureID, fixtureID, uuid.NewString(), 1}, MessageID: uuid.NewString(), OccurredAt: time.Now()}
	j.Payload.PlanRef = uuid.NewString()
	j.Payload.Dispatch = uuid.NewString()
	limits := map[string]any{"max_client_turns": 3, "max_tool_calls": 2, "session_duration_ms": 10000}
	plan := map[string]any{
		"schema_version": 1, "context": j.Scope, "seed": 12, "limits": limits,
		"versions":      Versions{fixtureID, fixtureID, fixtureID, fixtureID, fixtureID},
		"agent_adapter": "http-json-v1", "credential_revision_ref": fixtureID,
		"agent": map[string]any{"schema_version": 1, "adapter": "http-json-v1", "endpoint": "http://example.test", "credential_ref": fixtureID},
		"scenario": map[string]any{
			"schema_version": 1, "dataset_item_id": fixtureID, "title": "test", "modality": "text",
			"environment_version_id": fixtureID, "simulator_version_id": fixtureID, "template_id": "field-service",
			"template_version": "v1", "initial_fixture": map[string]any{}, "actor_customer_ref": "client-001",
			"virtual_clock": "2026-11-01T12:00:00Z", "business_timezone": "UTC", "client_goal": "request booking",
			"client_known_facts": map[string]any{}, "fault_rules": []any{}, "assertions": []any{map[string]any{"secret": "hidden"}},
			"limits": limits, "seed_policy": map[string]any{},
		},
		"environment": map[string]any{"schema_version": 1},
		"simulator":   map[string]any{"mode": "scripted"},
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	sha := sha256.Sum256(canonical)
	j.Payload.PlanHash = hex.EncodeToString(sha[:])
	return j, &pb.LeaseResponse{Accepted: true, RuntimePlanRef: j.Payload.PlanRef, RuntimePlanSha256: j.Payload.PlanHash,
		RuntimePlanJson: raw, RuntimeCredentials: map[string]string{"agent": testSecret}}
}

type testControl struct {
	plan           *pb.LeaseResponse
	claimed        atomic.Bool
	beats          atomic.Int32
	cancel, reject bool
	ttl            time.Duration
}

func (c *testControl) expires() string {
	ttl := c.ttl
	if ttl == 0 {
		ttl = time.Second
	}
	return time.Now().Add(ttl).Format(time.RFC3339Nano)
}

func (c *testControl) Claim(_ context.Context, r *pb.LeaseRequest) (*pb.LeaseResponse, error) {
	if !IDValid(r.WorkerId) || r.DispatchId == "" || r.Context.AttemptNo != 1 {
		return nil, errors.New("invalid claim")
	}
	if !c.claimed.CompareAndSwap(false, true) {
		return &pb.LeaseResponse{Reason: "STALE_ATTEMPT"}, nil
	}
	return &pb.LeaseResponse{Accepted: true, LeaseId: fixtureID, LeaseExpiresAt: c.expires(),
		RuntimePlanRef: c.plan.RuntimePlanRef, RuntimePlanSha256: c.plan.RuntimePlanSha256}, nil
}
func (c *testControl) Heartbeat(_ context.Context, r *pb.LeaseRequest) (*pb.LeaseResponse, error) {
	c.beats.Add(1)
	return &pb.LeaseResponse{Accepted: !c.reject, LeaseId: r.LeaseId, LeaseExpiresAt: c.expires(), CancelRequested: c.cancel}, nil
}
func (c *testControl) Plan(context.Context, *pb.PlanRequest) (*pb.LeaseResponse, error) {
	return c.plan, nil
}

type testPub struct {
	msgs         [][]byte
	kinds        []string
	failTerminal int
	attempted    [][]byte
}

func (p *testPub) Publish(ctx context.Context, kind, id string, b []byte) error {
	if !IDValid(id) || ctx.Err() != nil {
		return errors.New("cannot publish")
	}
	if strings.HasSuffix(kind, "executed.v1") && p.failTerminal > 0 {
		p.failTerminal--
		p.attempted = append(p.attempted, bytes.Clone(b))
		return errors.New("publish response lost")
	}
	p.kinds = append(p.kinds, kind)
	p.msgs = append(p.msgs, bytes.Clone(b))
	return nil
}

type testEvidence struct {
	data map[string][]byte
	fail bool
}

func (a *testEvidence) Upload(_ context.Context, _ Access, b Blob) (Ref, error) {
	if a.fail {
		return Ref{}, errors.New("upload failed")
	}
	raw, err := io.ReadAll(b.Data)
	if err != nil {
		return Ref{}, err
	}
	hash := sha256.Sum256(raw)
	if int64(len(raw)) != b.Size || hex.EncodeToString(hash[:]) != b.SHA {
		return Ref{}, errors.New("bad checksum")
	}
	a.data[b.Kind] = raw
	return Ref{uuid.NewString(), b.SHA, b.Size, b.MIME, b.Kind}, nil
}
func (a *testEvidence) Resolve(_ context.Context, _ Access, r Ref) error {
	if !r.Valid(r.Kind) {
		return errors.New("invalid reference")
	}
	return nil
}

type fakeAgent struct {
	reply func(context.Context, Input) (Response, error)
}

func (a fakeAgent) Open(_ context.Context, _ Scope, _ AgentConfig, secret string) (Agent, error) {
	if secret != testSecret {
		return nil, errors.New("incorrect credential")
	}
	return a, nil
}
func (a fakeAgent) Exchange(c context.Context, i Input) (Response, error) { return a.reply(c, i) }
func (fakeAgent) Close(context.Context) error                             { return nil }

type fakeSim struct{}

func (fakeSim) Open(_ context.Context, i SimulationInput) (Simulator, error) {
	if i.Goal != "request booking" || i.Seed != 12 || bytes.Contains(i.Definition, []byte("hidden")) {
		return nil, errors.New("invalid simulator context")
	}
	return fakeSim{}, nil
}
func (fakeSim) Next(_ context.Context, last []string) (Action, error) {
	if len(last) > 0 {
		return Action{Finished: true}, nil
	}
	return Action{Text: "Please book an appointment"}, nil
}
func (fakeSim) Close(context.Context) error { return nil }

type fakeEnvironment struct {
	unavailable bool
	finalized   *atomic.Bool
}

func (e fakeEnvironment) Open(context.Context, Scope, Setup) (Workspace, error) { return e, nil }
func (fakeEnvironment) Call(_ context.Context, c Call) Result {
	return Result{ID: c.ID, Status: 504, Body: json.RawMessage(`{"error":"timeout_after_save"}`)}
}
func (e fakeEnvironment) Finalize(context.Context) (Ref, Ref, error) {
	if e.finalized != nil {
		e.finalized.Store(true)
	}
	if e.unavailable {
		return Ref{}, Ref{}, ErrUnsupported
	}
	return Ref{uuid.NewString(), strings.Repeat("a", 64), 10, "application/json", "environment_snapshot"},
		Ref{uuid.NewString(), strings.Repeat("b", 64), 12, "application/x-ndjson", "environment_audit"}, nil
}

type timedOutEvidence struct{ Evidence }

func (timedOutEvidence) Upload(ctx context.Context, _ Access, _ Blob) (Ref, error) {
	<-ctx.Done()
	return Ref{}, ctx.Err()
}

func TestFinalizationSurvivesJournalTimeout(t *testing.T) {
	e, job, _, pub, _ := newEngine(t)
	var finalized atomic.Bool
	e.Environment = fakeEnvironment{finalized: &finalized}
	e.Evidence = timedOutEvidence{e.Evidence}
	e.CloseTimeout = 100 * time.Millisecond
	if e.Run(t.Context(), job) != Ack || !finalized.Load() || !strings.HasSuffix(pub.kinds[len(pub.kinds)-1], "failed.v1") {
		t.Fatal("journal timeout prevented CRM finalization or failed terminal event")
	}
}

func TestPlanCancellationPreventsOpeningWorkspace(t *testing.T) {
	e, job, ctl, pub, _ := newEngine(t)
	ctl.plan.CancelRequested = true
	e.Environment = nil
	if e.Run(t.Context(), job) != Ack || !strings.HasSuffix(pub.kinds[len(pub.kinds)-1], "cancelled.v1") {
		t.Fatal("accepted cancellation did not stop workspace initialization")
	}
}

func newEngine(t testing.TB) (*Engine, Job, *testControl, *testPub, *testEvidence) {
	t.Helper()
	j, plan := planFor(t)
	schemas, err := contracts.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctl := &testControl{plan: plan}
	pub := &testPub{}
	evidence := &testEvidence{data: make(map[string][]byte)}
	e := &Engine{
		Control: ctl, Publisher: pub, Evidence: evidence, Agents: fakeAgent{func(_ context.Context, i Input) (Response, error) {
			if i.Message != nil {
				return Response{Calls: []Call{{"tool-1", "create_order", json.RawMessage(`{"client_id":"client-001"}`), "original-key"}}}, nil
			}
			if len(i.Results) != 1 || i.Results[0].Status != 504 {
				return Response{}, errors.New("missing original CRM status")
			}
			return Response{Messages: []string{"timeout received", testSecret}, Finished: true}, nil
		}},
		Simulators: fakeSim{}, Environment: fakeEnvironment{}, Schemas: schemas,
		Metrics: new(Metrics), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		TempDir: t.TempDir(), EvidenceLimit: 1 << 20, PlanLimit: 1 << 20, HeartbeatInterval: 25 * time.Millisecond, CloseTimeout: time.Second,
	}
	return e, j, ctl, pub, evidence
}

func TestExecutionEvidenceAndDuplicate(t *testing.T) {
	e, j, ctl, p, a := newEngine(t)
	ctl.plan.RuntimeCredentials["agent"] = " " + testSecret + " "
	p.failTerminal = 1
	if e.Run(t.Context(), j) != Ack {
		t.Fatal("execution not acknowledged")
	}
	before := len(p.msgs)
	if e.Run(t.Context(), j) != Ack || len(p.msgs) != before {
		t.Fatal("stale command executed again")
	}
	if !strings.HasSuffix(p.kinds[0], "started.v1") || !strings.HasSuffix(p.kinds[before-1], "executed.v1") {
		t.Fatal(p.kinds)
	}
	if len(p.attempted) != 1 || !bytes.Equal(p.attempted[0], p.msgs[before-1]) {
		t.Fatal("terminal retry changed event identity")
	}
	if !bytes.Contains(a.data["tool_calls"], []byte("timeout_after_save")) || !bytes.Contains(a.data["tool_calls"], []byte("original-key")) {
		t.Fatalf("missing CRM evidence: %s", a.data["tool_calls"])
	}
	if bytes.Contains(a.data["conversation"], []byte(testSecret)) || !bytes.Contains(a.data["conversation"], []byte("[REDACTED]")) {
		t.Fatal("credential not redacted")
	}
	if !bytes.Contains(a.data["execution_manifest"], []byte(`"audio_refs":[]`)) ||
		!bytes.Contains(a.data["execution_manifest"], []byte(`"cost_status":"unknown"`)) {
		t.Fatalf("invalid manifest: %s", a.data["execution_manifest"])
	}
	last := 0
	for _, msg := range p.msgs {
		var v struct {
			Payload struct {
				Seq int `json:"session_seq"`
			} `json:"payload"`
		}
		if json.Unmarshal(msg, &v) != nil || v.Payload.Seq <= last {
			t.Fatalf("non-increasing event sequence: %s", msg)
		}
		last = v.Payload.Seq
	}
}

func TestExternalBlockersAndFaults(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(*Engine, *testEvidence)
		expected string
	}{
		{"missing simulator", func(e *Engine, _ *testEvidence) { e.Simulators = nil }, "DEPENDENCY_UNAVAILABLE"},
		{"missing Environment publisher", func(e *Engine, _ *testEvidence) { e.Environment = fakeEnvironment{unavailable: true} }, "DEPENDENCY_UNAVAILABLE"},
		{"failed PUT", func(_ *Engine, a *testEvidence) { a.fail = true }, "ARTIFACT_UPLOAD_FAILED"},
		{"agent protocol", func(e *Engine, _ *testEvidence) {
			e.Agents = fakeAgent{func(context.Context, Input) (Response, error) { return Response{}, ErrProtocol }}
		}, "agent_protocol_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, j, _, p, a := newEngine(t)
			tc.setup(e, a)
			if e.Run(t.Context(), j) != Ack {
				t.Fatal("missing terminal acknowledgement")
			}
			last := p.msgs[len(p.msgs)-1]
			if !bytes.Contains(last, []byte(tc.expected)) {
				t.Fatalf("terminal outcome: %s", last)
			}
			if tc.expected != "agent_protocol_error" && bytes.Contains(last, []byte("session.executed")) {
				t.Fatal("published false success")
			}
		})
	}
}

func TestHeartbeatStopsBlockedRequest(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelled), func(t *testing.T) {
			e, j, c, p, _ := newEngine(t)
			c.cancel, c.reject = cancelled, !cancelled
			e.Agents = fakeAgent{func(ctx context.Context, _ Input) (Response, error) {
				select {
				case <-ctx.Done():
					return Response{}, ctx.Err()
				case <-time.After(time.Second):
					return Response{}, errors.New("heartbeat did not interrupt request")
				}
			}}
			if disposition := e.Run(t.Context(), j); (disposition == Ack) != cancelled {
				t.Fatalf("disposition %d", disposition)
			}
			if c.beats.Load() == 0 {
				t.Fatal("no heartbeat during request")
			}
			terminal := p.kinds[len(p.kinds)-1]
			if cancelled && !strings.HasSuffix(terminal, "cancelled.v1") {
				t.Fatal(terminal)
			}
			if !cancelled && e.Metrics.LeaseLost.Load() != 1 {
				t.Fatal("lease loss not observed")
			}
		})
	}
}

func TestExpiredLeaseFencesSideEffectsBeforeMonitorRuns(t *testing.T) {
	var deadline atomic.Int64
	deadline.Store(time.Now().Add(-time.Second).UnixNano())
	ctx := context.WithValue(t.Context(), leaseClockKey{}, &deadline)
	if !errors.Is(CheckLease(ctx), errLease) {
		t.Fatal("side effects allowed under expired lease")
	}
	deadline.Store(time.Now().Add(time.Second).UnixNano())
	if err := CheckLease(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPlanIntegrity(t *testing.T) {
	e, j, c, _, _ := newEngine(t)
	if _, err := ParsePlan(j, c.plan, e.Schemas, e.PlanLimit); err != nil {
		t.Fatal(err)
	}
	c.plan.RuntimePlanJson = bytes.Replace(c.plan.RuntimePlanJson, []byte(`"seed":12`), []byte(`"seed":13`), 1)
	if _, err := ParsePlan(j, c.plan, e.Schemas, e.PlanLimit); err == nil {
		t.Fatal("tampered plan accepted")
	}
}

func TestPlanUsesVerifiedNumbers(t *testing.T) {
	e, job, ctl, _, _ := newEngine(t)
	raw := bytes.ReplaceAll(ctl.plan.RuntimePlanJson, []byte(`"max_client_turns":3`),
		[]byte(`"budget_minor":9007199254740992,"max_client_turns":3`))
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	job.Payload.PlanHash = hex.EncodeToString(digest[:])
	ctl.plan.RuntimePlanSha256 = job.Payload.PlanHash
	ctl.plan.RuntimePlanJson = bytes.ReplaceAll(raw, []byte("9007199254740992"), []byte("9007199254740993"))
	plan, err := ParsePlan(job, ctl.plan, e.Schemas, e.PlanLimit)
	if err != nil || plan.Limits.Budget == nil || *plan.Limits.Budget != 9007199254740992 {
		t.Fatalf("execution did not use the verified budget: %v, %v", plan.Limits.Budget, err)
	}
}

func TestStrictJSONProtectsEvidence(t *testing.T) {
	for _, raw := range []string{
		`{"value":{"x":"\u0000"}}`,
		`{"value":{"\u0000":"x"}}`,
		`{"value":{"x":1e-999999}}`,
		`{"value":{"x":[` + strings.Repeat("[", 64) + "0" + strings.Repeat("]", 64) + `]}}`,
		`{"value":{"x":1,"\u0078":2}}`,
	} {
		var value json.RawMessage
		if err := DecodeStrict([]byte(raw), &value); err == nil {
			t.Errorf("accepted unsafe evidence: %s", raw)
		}
	}
	var value json.RawMessage
	if err := DecodeStrict([]byte(`{"value":{"ID":"a","id":"b","x":"\\u0000"}}`), &value); err != nil {
		t.Fatalf("valid opaque fields were rejected: %v", err)
	}
}

func TestJournalCompactsAndRedactsEvidence(t *testing.T) {
	job, _ := planFor(t)
	pub := &testPub{}
	conversation, err := openJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer conversation.close()
	const secret = "1234567890123"
	r := recorder{events: newEvents(t.Context(), job, pub), conversation: conversation, tools: &journal{}, limit: 1 << 20, secret: secret}
	payload := map[string]any{
		"message": strings.Repeat("<", 12000), "cost_minor": json.Number(secret),
		"echo" + secret: "private field value", "echo[REDACTED]": "existing field value",
		"other": json.Number("1234567890123456789"),
	}
	// The other number intentionally shares the token's prefix, so it too must
	// be hidden. A different large integer must remain exact.
	payload["exact"] = json.Number("9007199254740993")
	if err := r.append(t.Context(), conversation, "custom.record", payload); err != nil {
		t.Fatal(err)
	}
	blob, err := conversation.blob("conversation")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(blob.Data)
	if err != nil || len(raw) > MaxEvent || len(pub.msgs) != 1 {
		t.Fatalf("evidence was not recorded and published: %d bytes, %d events, %v", len(raw), len(pub.msgs), err)
	}
	for _, data := range [][]byte{raw, pub.msgs[0]} {
		if bytes.Contains(data, []byte(secret)) || !bytes.Contains(data, []byte(`"exact":9007199254740993`)) ||
			!bytes.Contains(data, []byte("private field value")) || !bytes.Contains(data, []byte("existing field value")) {
			t.Fatal("redaction leaked the credential or discarded evidence")
		}
	}
}

func TestRedactionPreservesRunnerMetadata(t *testing.T) {
	for _, tc := range []struct{ secret, wantText string }{{"1", "secret *"}, {"t", "secre* *"}} {
		fields := map[string]any{"text": "secret " + tc.secret, "turn": json.Number("1")}
		result := redactPayload("client.message", fields, tc.secret).(map[string]any)
		if result["turn"] != json.Number("1") || result["text"] != tc.wantText {
			t.Fatalf("unexpected redaction: %v", result)
		}
	}
}

func FuzzDecodeJob(f *testing.F) {
	f.Add([]byte("{}"))
	f.Add([]byte(`{"attempt_no":4294967296}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		j, err := DecodeJob(CommandSubject, raw)
		if err == nil && !j.Scope.Valid() {
			t.Fatal("accepted invalid scope")
		}
	})
}

func BenchmarkPlanAndExecution(b *testing.B) {
	e, j, c, _, _ := newEngine(b)
	b.Run("frozen_plan", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := ParsePlan(j, c.plan, e.Schemas, e.PlanLimit); err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, parallel := range []int{1, 10, 50, 100} {
		b.Run(fmt.Sprintf("sessions_%d", parallel), func(b *testing.B) {
			jobs := make([]Job, parallel)
			plans := make([]*pb.LeaseResponse, parallel)
			for i := range jobs {
				jobs[i], plans[i] = planFor(b)
			}
			b.ReportAllocs()
			var elapsed time.Duration
			var samples []time.Duration
			var peak int
			var errs atomic.Int64
			var total int
			cpuBefore := cpuSeconds()
			var memStart, memAfter runtime.MemStats
			runtime.ReadMemStats(&memStart)
			for b.Loop() {
				var group sync.WaitGroup
				times := make([]time.Duration, parallel)
				for i := range parallel {
					group.Go(func() {
						start := time.Now()
						engine := *e
						engine.Control = &testControl{plan: plans[i], ttl: 30 * time.Second}
						engine.Publisher = &testPub{}
						engine.Evidence = &testEvidence{data: make(map[string][]byte)}
						if engine.Run(b.Context(), jobs[i]) != Ack {
							errs.Add(1)
						}
						times[i] = time.Since(start)
					})
				}
				peak = max(peak, runtime.NumGoroutine())
				group.Wait()
				if len(samples) < 10000 {
					samples = append(samples, times...)
				}
				total += parallel
			}
			elapsed = b.Elapsed()
			cpuAfter := cpuSeconds()
			runtime.ReadMemStats(&memAfter)
			slices.Sort(samples)
			for _, pct := range []int{50, 95, 99} {
				b.ReportMetric(float64(samples[(len(samples)-1)*pct/100].Microseconds()), fmt.Sprintf("p%d-us", pct))
			}
			b.ReportMetric(float64(total)/elapsed.Seconds(), "sessions/s")
			b.ReportMetric(float64(peak), "goroutines-sampled")
			b.ReportMetric(float64(runtime.NumGoroutine()), "goroutines-after")
			b.ReportMetric(float64(memAfter.HeapInuse), "heap-B-after")
			b.ReportMetric(float64(memAfter.TotalAlloc-memStart.TotalAlloc)/float64(total), "B/session")
			b.ReportMetric((cpuAfter-cpuBefore)/elapsed.Seconds(), "runtime-CPU-s/s")
			if errs.Load() != 0 {
				b.Fatalf("%d failed sessions", errs.Load())
			}
		})
	}
}

func cpuSeconds() float64 {
	sample := []metrics.Sample{{Name: "/cpu/classes/total:cpu-seconds"}, {Name: "/cpu/classes/idle:cpu-seconds"}}
	metrics.Read(sample)
	return sample[0].Value.Float64() - sample[1].Value.Float64()
}
