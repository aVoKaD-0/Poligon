package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	pb "poligon/environment/gen/controlv1"
	env "poligon/environment/gen/environmentv1"
	"poligon/environment/internal/handler"
	"poligon/environment/internal/service"
	"poligon/environment/internal/store"
	"poligon/environment/pkg/api"
)

var testScope = Scope{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003", 1}

func serveRPC(t *testing.T, server *grpc.Server) *grpc.ClientConn {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(l)
	t.Cleanup(server.Stop)
	c, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func auth(ctx context.Context, token string) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
}
func makeRequest(method, url, token, id string, scope Scope, body io.Reader) *http.Request {
	r, err := http.NewRequest(method, url, body)
	if err != nil {
		panic(err)
	}
	b, err := json.Marshal(scope)
	if err != nil {
		panic(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Execution-Context", string(b))
	r.Header.Set("X-Session-ID", id)
	return r
}
func httpCall(t *testing.T, r *http.Request, want int) map[string]any {
	t.Helper()
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != want {
		t.Fatalf("HTTP %s %s: %d (%s), %v", r.Method, r.URL.Path, res.StatusCode, b, err)
	}
	var data map[string]any
	if err = json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	return data
}
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../contracts/fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestValidationRPC(t *testing.T) {
	s := New(store.NewManager(), "runner", "platform", nil, "control")
	c := env.NewEnvironmentServiceClient(serveRPC(t, s.GRPC()))
	ctx := auth(t.Context(), "platform")
	var scenario map[string]any
	if err := json.Unmarshal(fixture(t, "validate-request.json"), &scenario); err != nil {
		t.Fatal(err)
	}
	toStruct := func(m map[string]any) *structpb.Struct {
		v, err := structpb.NewStruct(m)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	req := toStruct(scenario)
	resp, err := c.ValidateTemplate(ctx, req)
	if err != nil || len(resp.AsMap()["errors"].([]any)) != 0 {
		t.Fatalf("valid scenario: %v, %v", resp, err)
	}
	for _, token := range []string{"runner", "control", "wrong"} {
		if _, err = c.ValidateTemplate(auth(t.Context(), token), req); status.Code(err) != codes.Unauthenticated {
			t.Fatal("wrong validation role", err)
		}
	}
	cap := map[string]any{"version_id": versionID, "modality": "text", "kind": "environment", "scenario": scenario}
	resp, err = c.GetCapabilities(ctx, toStruct(cap))
	if err != nil || resp.AsMap()["supported"] != true {
		t.Fatal(resp, err)
	}
	cap["version_id"] = "unknown"
	resp, err = c.GetCapabilities(ctx, toStruct(cap))
	if err != nil || resp.AsMap()["supported"] != false {
		t.Fatal(resp, err)
	}
	for _, bad := range []struct {
		k string
		v any
	}{{"schema_version", float64(2)}, {"modality", "voice"}, {"environment_version_id", "unknown"}, {"template_id", "service-booking"}, {"template_version", "v2"}, {"initial_fixture", map[string]any{"orders": []any{}}}, {"initial_fixture", map[string]any{"slots": "x"}}, {"initial_fixture", map[string]any{"clients": []any{map[string]any{}}}}, {"fault_rules", []any{map[string]any{"fault_type": "unknown"}}}, {"limits", map[string]any{"unsafe": float64(1 << 54)}}} {
		invalid := req.AsMap()
		invalid[bad.k] = bad.v
		res, err := c.ValidateTemplate(ctx, toStruct(invalid))
		if err != nil || len(res.AsMap()["errors"].([]any)) == 0 {
			t.Fatalf("accepted %s=%v: %v", bad.k, bad.v, err)
		}
	}
}

type registry struct {
	pb.UnimplementedControlServiceServer
	mu       sync.Mutex
	url      string
	prepared map[string]*pb.ArtifactRequest
	bytes    map[string][]byte
	lost     bool
}

func (r *registry) PrepareArtifactUpload(ctx context.Context, req *pb.ArtifactRequest) (*pb.ArtifactResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Join(metadata.ValueFromIncomingContext(ctx, "authorization"), "") != "Bearer control" || req.Producer != "environment" || scopeOf(req.Context) != testScope {
		return nil, status.Error(codes.PermissionDenied, "scope or identity")
	}
	if _, exists := r.prepared[req.Kind]; exists {
		return nil, status.Error(codes.AlreadyExists, "duplicate preparation")
	}
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(r.prepared)+1)
	clone := proto.CloneOf(req)
	clone.UploadId = id
	clone.ObjectKey = "evidence/" + id
	r.prepared[req.Kind] = clone
	return &pb.ArtifactResponse{UploadId: id, ObjectKey: clone.ObjectKey, SignedUrl: r.url + "/" + req.Kind, RequiredHeaders: map[string]string{"Content-Type": req.Mime, "X-Test-Signed": "required"}}, nil
}
func (r *registry) RegisterArtifact(ctx context.Context, req *pb.ArtifactRequest) (*pb.ArtifactResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Join(metadata.ValueFromIncomingContext(ctx, "authorization"), "") != "Bearer control" || !proto.Equal(r.prepared[req.Kind], req) || digest(r.bytes[req.Kind]) != req.Sha256 || int64(len(r.bytes[req.Kind])) != req.SizeBytes {
		return nil, status.Error(codes.InvalidArgument, "registration mismatch")
	}
	if req.Kind == "environment_audit" && !r.lost {
		r.lost = true
		return nil, status.Error(codes.Unavailable, "registration response lost")
	}
	return &pb.ArtifactResponse{ArtifactRef: &pb.ArtifactRef{ArtifactId: req.UploadId, Kind: req.Kind, Mime: req.Mime, SizeBytes: req.SizeBytes, Sha256: req.Sha256}}, nil
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for attempt state")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestAttemptLifecycle(t *testing.T) {
	failUpload := true
	reg := &registry{prepared: map[string]*pb.ArtifactRequest{}, bytes: map[string][]byte{}}
	objectStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil || r.Method != "PUT" || r.Header.Get("X-Test-Signed") != "required" {
			w.WriteHeader(400)
			return
		}
		reg.mu.Lock()
		defer reg.mu.Unlock()
		kind := strings.TrimPrefix(r.URL.Path, "/")
		if failUpload {
			failUpload = false
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if _, ok := reg.bytes[kind]; ok {
			t.Error("repeated PUT after successful upload")
		}
		reg.bytes[kind] = b
	}))
	defer objectStore.Close()
	reg.url = objectStore.URL
	ctrl := grpc.NewServer()
	pb.RegisterControlServiceServer(ctrl, reg)
	m := store.NewManager()
	s := New(m, "runner", "platform", pb.NewControlServiceClient(serveRPC(t, ctrl)), "control")
	crm := httptest.NewServer(s.HTTP(handler.NewHandler(m, service.NewService(m))))
	defer crm.Close()
	c := env.NewEnvironmentServiceClient(serveRPC(t, s.GRPC()))
	opening := fixture(t, "open-request.json")
	const id = "crm-1"
	req := func(method, path string, scope Scope, b []byte) *http.Request {
		return makeRequest(method, crm.URL+"/api/v1/"+path, "runner", id, scope, bytes.NewReader(b))
	}
	unauthorized := req("POST", "attempts", testScope, opening)
	unauthorized.Header.Set("Authorization", "Bearer platform")
	httpCall(t, unauthorized, 401)
	httpCall(t, req("POST", "attempts", testScope, opening), 201)
	httpCall(t, req("POST", "attempts", testScope, bytes.Replace(opening, []byte("crm-1"), []byte("conflict"), 1)), 409)
	scope2 := testScope
	scope2.Attempt++
	var second map[string]any
	json.Unmarshal(opening, &second)
	second["context"] = scope2
	body, _ := json.Marshal(second)
	httpCall(t, req("POST", "attempts", scope2, body), 409)
	second["session_id"] = "crm-2"
	body, _ = json.Marshal(second)
	httpCall(t, req("POST", "attempts", scope2, body), 201)
	httpCall(t, req("GET", "state", scope2, nil), 403)
	httpCall(t, req("GET", "sessions/crm-2/state", testScope, nil), 403)
	badToken := req("GET", "state", testScope, nil)
	badToken.Header.Del("Authorization")
	httpCall(t, badToken, 401)
	order := []byte(`{"client_id":"client-001","address":"Test","date":"2026-11-20","time_window":"18:00-19:00"}`)
	// Leave a real accepted mutation waiting for its HTTP body while Finalize runs.
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()
	pending := makeRequest("POST", crm.URL+"/api/v1/orders", "runner", id, testScope, reader)
	done := make(chan int, 1)
	go func() {
		res, err := http.DefaultClient.Do(pending)
		if err != nil {
			done <- 0
			return
		}
		res.Body.Close()
		done <- res.StatusCode
	}()
	a := s.lookup(testScope, id)
	eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.active == 1 })
	request := &env.FinalizeAttemptRequest{Context: testScope.proto(), EnvironmentSessionId: id}
	if _, err := c.FinalizeAttempt(auth(t.Context(), "platform"), request); status.Code(err) != codes.Unauthenticated {
		t.Fatal("wrong finalize role", err)
	}
	ctx := auth(t.Context(), "runner")
	if _, err := c.FinalizeAttempt(ctx, &env.FinalizeAttemptRequest{Context: scope2.proto(), EnvironmentSessionId: id}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("wrong scope", err)
	}
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	if _, err := c.FinalizeAttempt(short, request); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal("finalize ignored caller deadline", err)
	}
	cancel()
	first := make(chan error, 1)
	go func() { _, err := c.FinalizeAttempt(ctx, request); first <- err }()
	eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.closing })
	httpCall(t, req("POST", "reset", testScope, nil), 409)
	if _, err := writer.Write(order); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if got := <-done; got != 504 {
		t.Fatalf("delayed response %d", got)
	}
	if err := <-first; status.Code(err) != codes.Unavailable {
		t.Fatalf("storage failure should be reported: %v", err)
	}
	partial, err := c.FinalizeAttempt(ctx, request)
	if status.Code(err) != codes.Unavailable || partial != nil {
		t.Fatal("lost register response should remain an error", partial, err)
	}
	res, err := c.FinalizeAttempt(ctx, request)
	if err != nil || !res.GetAuditComplete() || !res.GetSnapshotFinalized() {
		t.Fatal(res, err)
	}
	again, err := c.FinalizeAttempt(ctx, request)
	if err != nil || !proto.Equal(res, again) {
		t.Fatal("unstable finalization", again, err)
	}
	var replays sync.WaitGroup
	for i := 0; i < 8; i++ {
		replays.Add(1)
		go func() {
			defer replays.Done()
			reply, err := c.FinalizeAttempt(ctx, request)
			if err != nil || !proto.Equal(reply, res) {
				t.Error("concurrent finalize changed refs", err)
			}
		}()
	}
	replays.Wait()
	opened := httpCall(t, req("POST", "attempts", testScope, opening), 200)
	if opened["snapshot"].(map[string]any)["total_orders_count"] != float64(1) {
		t.Fatal("reopening lost order")
	}
	for _, path := range []string{"orders", "sessions", "sessions/crm-1/reset", "sessions/crm-1/faults"} {
		httpCall(t, req("POST", path, testScope, order), 409)
	}
	other := makeRequest("GET", crm.URL+"/api/v1/state", "runner", "crm-2", scope2, nil)
	if data := httpCall(t, other, 200); data["total_orders_count"] != float64(0) {
		t.Fatal("attempt isolation")
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	snapshot, audit := reg.bytes["environment_snapshot"], reg.bytes["environment_audit"]
	if digest(snapshot) != res.SnapshotRef.Sha256 || digest(audit) != res.AuditRef.Sha256 {
		t.Fatal("registered bytes mismatch")
	}
	var state api.StateSnapshot
	if err = json.Unmarshal(snapshot, &state); err != nil || len(state.Orders) != 1 {
		t.Fatal("snapshot missing mutation")
	}
	lines := bytes.Split(bytes.TrimSpace(audit), []byte("\n"))
	if len(lines) < 3 {
		t.Fatal("incomplete audit")
	}
	var failed, marker api.AuditEvent
	if err = json.Unmarshal(lines[len(lines)-2], &failed); err != nil || failed.ResponseStatus != 504 {
		t.Fatal("audit missing delayed 504")
	}
	if err = json.Unmarshal(lines[len(lines)-1], &marker); err != nil || marker.Action != "FINALIZE_ATTEMPT" || marker.AfterState.(map[string]any)["snapshot_sha256"] != digest(snapshot) {
		t.Fatal("invalid finalization marker")
	}
}

func TestPublishedArtifactFixtures(t *testing.T) {
	var response env.FinalizeAttemptResponse
	if err := protojson.Unmarshal(fixture(t, "finalize-response.json"), &response); err != nil {
		t.Fatal(err)
	}
	for file, ref := range map[string]*pb.ArtifactRef{"snapshot.json": response.SnapshotRef, "audit.ndjson": response.AuditRef} {
		raw := fixture(t, file)
		if int64(len(raw)) != ref.GetSizeBytes() || digest(raw) != ref.GetSha256() {
			t.Fatalf("reference does not match %s", file)
		}
	}
	lines := bytes.Split(bytes.TrimSpace(fixture(t, "audit.ndjson")), []byte("\n"))
	var marker struct {
		Action string
		After  struct {
			Scope       Scope  `json:"context"`
			SnapshotSHA string `json:"snapshot_sha256"`
		} `json:"after_state"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Action != "FINALIZE_ATTEMPT" || marker.After.SnapshotSHA != response.SnapshotRef.Sha256 || marker.After.Scope != scopeOf(response.Context) {
		t.Fatal("finalization marker mismatch")
	}
}
