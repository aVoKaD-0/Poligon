package integration

import (
	"bytes"
	"encoding/json"

	"os"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	env "poligon/environment/gen/environmentv1"
)

// The caller supplies a disposable attempt assigned by the real local Platform.
func TestPublishedControl(t *testing.T) {
	raw := os.Getenv("ENVIRONMENT_TEST_CONTEXT")
	if raw == "" {
		t.Skip("requires provisioned local Platform execution attempt")
	}
	var scope Scope
	if err := json.Unmarshal([]byte(raw), &scope); err != nil || !scope.valid() {
		t.Fatal("invalid ENVIRONMENT_TEST_CONTEXT")
	}
	addr, token := os.Getenv("ENVIRONMENT_GRPC_ADDR"), os.Getenv("RUNNER_SERVICE_TOKEN")
	if addr == "" || token == "" {
		t.Fatal("Environment address and Runner token required")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := env.NewEnvironmentServiceClient(conn)
	crmURL := "http://" + os.Getenv("ENVIRONMENT_HTTP_ADDR")
	id := "crm-" + scope.Session
	var req map[string]any
	if err = json.Unmarshal(fixture(t, "open-request.json"), &req); err != nil {
		t.Fatal(err)
	}
	req["context"] = scope
	req["session_id"] = id
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string, body []byte, want int) map[string]any {
		return httpCall(t, makeRequest("POST", crmURL+"/api/v1/"+path, token, id, scope, bytes.NewReader(body)), want)
	}
	call("attempts", data, 201)
	call("orders", []byte(`{"client_id":"client-001","address":"Test","date":"2026-11-20","time_window":"18:00-19:00"}`), 504)
	opened := call("attempts", data, 200)
	if opened["snapshot"].(map[string]any)["total_orders_count"] != float64(1) {
		t.Fatal("opening replay lost mutation")
	}
	ctx := auth(t.Context(), token)
	final := &env.FinalizeAttemptRequest{Context: scope.proto(), EnvironmentSessionId: id}
	res, err := c.FinalizeAttempt(ctx, final)
	if os.Getenv("ENVIRONMENT_TEST_EXPIRED") == "1" {
		if status.Code(err) != codes.PermissionDenied || res != nil {
			t.Fatal("expired lease accepted", res, err)
		}
		return
	}
	if err != nil || !res.GetSnapshotFinalized() || !res.GetAuditComplete() {
		t.Fatal("registration failed", err)
	}
	again, err := c.FinalizeAttempt(ctx, final)
	if err != nil || !proto.Equal(res, again) {
		t.Fatal("unstable refs", err)
	}
}
