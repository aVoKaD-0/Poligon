package platform

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	pb "github.com/poligon/platform/gen/controlv1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Fake S3 is confined to tests; production always uses the configured S3 client.
// Tests exercise SDK PUT/GET paths and checksum/registry semantics, not S3 auth.
func fakeS3(t *testing.T) *ArtifactStore {
	t.Helper()
	var mu sync.Mutex
	objects := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "PUT":
			b, e := io.ReadAll(r.Body)
			if e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
			if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
				var decoded bytes.Buffer
				br := bufio.NewReader(bytes.NewReader(b))
				for {
					line, e := br.ReadString('\n')
					if e != nil {
						t.Error(e)
						break
					}
					size, e := strconv.ParseInt(strings.Split(strings.TrimSpace(line), ";")[0], 16, 64)
					if e != nil {
						t.Error(e)
						break
					}
					if size == 0 {
						break
					}
					if _, e = io.CopyN(&decoded, br, size); e != nil {
						t.Error(e)
						break
					}
					_, _ = br.ReadString('\n')
				}
				b = decoded.Bytes()
			}
			objects[r.URL.Path] = b
			w.Header().Set("ETag", `"fake"`)
			w.WriteHeader(200)
		case "GET":
			b, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", stringNumber(len(b)))
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("ETag", `"fake"`)
			_, _ = w.Write(b)
		case "DELETE":
			delete(objects, r.URL.Path)
			w.WriteHeader(204)
		default:
			w.WriteHeader(405)
		}
	}))
	t.Cleanup(server.Close)
	client, e := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("test-key", "test-secret", ""), Secure: false, Region: "us-east-1"})
	if e != nil {
		t.Fatal(e)
	}
	return &ArtifactStore{client, client, "evidence"}
}
func claimFixture(t *testing.T, f *fixture) (context.Context, *Control, *pb.LeaseRequest) {
	t.Helper()
	run := f.start(t)
	ctx := context.WithValue(context.Background(), ctxKey("service"), "runner")
	var sid, dispatch string
	if e := f.a.DB.Pool.QueryRow(ctx, "SELECT s.id::text,a.dispatch_id::text FROM sessions s JOIN execution_attempts a ON a.session_id=s.id WHERE s.run_id=$1", str(run, "run_id")).Scan(&sid, &dispatch); e != nil {
		t.Fatal(e)
	}
	r := &pb.LeaseRequest{Context: &pb.ExecutionContext{ProjectId: f.p, RunId: str(run, "run_id"), SessionId: sid, AttemptNo: 1}, WorkerId: ID(), DispatchId: dispatch}
	c := &Control{App: f.a}
	lease, e := c.ClaimExecution(ctx, r)
	if e != nil || !lease.Accepted {
		t.Fatalf("claim %v %v", lease, e)
	}
	r.LeaseId = lease.LeaseId
	return ctx, c, r
}
func uploadArtifact(t *testing.T, c *Control, ctx context.Context, lease *pb.LeaseRequest, kind string, b []byte) *pb.ArtifactRef {
	t.Helper()
	req := &pb.ArtifactRequest{Context: lease.Context, WorkerId: lease.WorkerId, LeaseId: lease.LeaseId, EvaluationId: lease.EvaluationId, Producer: service(ctx), Kind: kind, SizeBytes: int64(len(b)), Sha256: hash(b), Mime: "application/json"}
	grant, e := c.PrepareArtifactUpload(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	put, e := http.NewRequest("PUT", grant.SignedUrl, strings.NewReader(string(b)))
	if e != nil {
		t.Fatal(e)
	}
	for k, v := range grant.RequiredHeaders {
		put.Header.Set(k, v)
	}
	res, e := http.DefaultClient.Do(put)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("PUT failed")
	}
	req.UploadId, req.ObjectKey = grant.UploadId, grant.ObjectKey
	registered, e := c.RegisterArtifact(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	again, e := c.RegisterArtifact(ctx, req)
	if e != nil || again.ArtifactRef.ArtifactId != registered.ArtifactRef.ArtifactId {
		t.Fatal("RegisterArtifact is not idempotent")
	}
	req.Sha256 = strings.Repeat("f", 64)
	if _, e = c.RegisterArtifact(ctx, req); e == nil {
		t.Fatal("re-registration accepted changed metadata")
	}
	put, _ = http.NewRequest("PUT", grant.SignedUrl, strings.NewReader(strings.Repeat("x", len(b))))
	for k, v := range grant.RequiredHeaders {
		put.Header.Set(k, v)
	}
	res, e = http.DefaultClient.Do(put)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	read, e := c.App.Store.read(ctx, grant.ObjectKey, int64(len(b)), hash(b))
	if e != nil || string(read) != string(b) {
		t.Fatal("reusing staging PUT changed registered artifact", e)
	}
	return registered.ArtifactRef
}
func refObject(ref *pb.ArtifactRef) Object {
	b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(ref)
	return obj(b)
}
func TestExecutedAssessmentAndImmutableEvidence(t *testing.T) {
	f := newFixture(t)
	f.a.Store = fakeS3(t)
	ctx, c, lease := claimFixture(t, f)
	ec := lease.Context
	contextBody := contextMap(ec)
	contextBody["schema_version"] = 1
	refs := map[string]any{}
	for _, pair := range []struct{ kind, field, service string }{{"conversation", "conversation_ref", "runner"}, {"tool_calls", "tool_calls_ref", "runner"}, {"environment_snapshot", "environment_snapshot_ref", "environment"}, {"environment_audit", "environment_audit_ref", "environment"}} {
		work := context.WithValue(ctx, ctxKey("service"), pair.service)
		record := obj(encode(contextBody))
		if pair.kind == "conversation" || pair.kind == "tool_calls" {
			record["producer_event_id"] = ID()
			seq := 1
			if pair.kind == "tool_calls" {
				seq = 2
			}
			record["session_seq"] = seq
			record["event_kind"] = "agent.message"
		}
		refs[pair.field] = refObject(uploadArtifact(t, c, work, lease, pair.kind, encode(record)))
	}
	var plan []byte
	if e := f.a.DB.Pool.QueryRow(ctx, "SELECT plan FROM execution_attempts WHERE session_id=$1", ec.SessionId).Scan(&plan); e != nil {
		t.Fatal(e)
	}
	manifest := obj(encode(contextBody))
	for k, v := range refs {
		manifest[k] = v
	}
	manifest["versions"] = obj(plan)["versions"]
	manifest["seed"] = 42
	manifest["execution_status"] = "executed"
	manifest["termination_reason"] = "completed"
	manifest["audio_refs"] = []any{}
	manifest["usage"] = Object{"input_tokens": 0, "output_tokens": 0, "audio_ms": 0}
	manifest["timestamps"] = Object{"started_at": "2026-10-29T10:00:00Z", "finished_at": "2026-10-29T10:01:00Z"}
	manifestRef := uploadArtifact(t, c, ctx, lease, "execution_manifest", encode(manifest))
	event := Envelope{SchemaVersion: 1, MessageID: ID(), ProducerEventID: ID(), Type: "session.executed.v1", Producer: "runner", OccurredAt: utc(), ProjectID: ec.ProjectId, RunID: ec.RunId, SessionID: ec.SessionId, AttemptNo: 1, Payload: Object{"session_seq": 5, "manifest_ref": refObject(manifestRef), "manifest_sha256": manifestRef.Sha256}}
	for i := 0; i < 2; i++ {
		if e := f.a.ApplyEvent(ctx, "poligon.evt.session.executed.v1", encode(event)); e != nil {
			t.Fatal(e)
		}
	}
	var eval string
	var count int
	if e := f.a.DB.Pool.QueryRow(ctx, "SELECT count(*),min(id::text) FROM evaluation_jobs WHERE session_id=$1", ec.SessionId).Scan(&count, &eval); e != nil || count != 1 {
		t.Fatal("duplicate evaluation created", e)
	}
	intel := context.WithValue(ctx, ctxKey("service"), "intelligence")
	er := &pb.LeaseRequest{Context: ec, WorkerId: ID(), EvaluationId: eval}
	el, e := c.ClaimEvaluation(intel, er)
	if e != nil || !el.Accepted {
		t.Fatal(el, e)
	}
	er.LeaseId = el.LeaseId
	assessment := obj(encode(contextBody))
	assessment["evaluation_id"] = eval
	assessment["evaluator_version_id"] = "a0000000-0000-4000-8000-000000000003"
	assessment["simulation_validity"] = "valid"
	assessment["manifest_sha256"] = manifestRef.Sha256
	assessment["scores"] = []any{}
	assessment["completeness"] = Object{"conversation": true, "tools": true, "environment": true, "audio": nil}
	assessment["usage"] = Object{"provider_cost_status": "unknown"}
	assessment["verdict"] = "fail"
	assessment["assertions"] = []any{Object{"assertion_id": "one-booking", "rule_id": "booking.count.v1", "required": true, "status": "fail", "reason": "Two bookings", "evidence": []any{Object{"artifact_id": sub(refs, "environment_snapshot_ref")["artifact_id"], "json_pointer": "/bookings/1"}}}}
	assessmentRef := uploadArtifact(t, c, intel, er, "assessment", encode(assessment))
	ev := Envelope{SchemaVersion: 1, MessageID: ID(), ProducerEventID: ID(), Type: "evaluation.completed.v1", Producer: "intelligence", OccurredAt: utc(), ProjectID: ec.ProjectId, RunID: ec.RunId, SessionID: ec.SessionId, AttemptNo: 1, Payload: Object{"evaluation_id": eval, "verdict": "fail", "assessment_ref": refObject(assessmentRef), "assessment_sha256": assessmentRef.Sha256}}
	if e = f.a.ApplyEvent(intel, "poligon.evt.evaluation.completed.v1", encode(ev)); e != nil {
		t.Fatal(e)
	}
	view, e := getRun(ctx, f.a.DB.Pool, f.p, ec.RunId)
	if e != nil || str(view, "status") != "completed" || str(view, "verdict") != "fail" {
		t.Fatal("agent defect must be completed/fail", view, e)
	}
}
