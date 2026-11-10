package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/poligon/runner/internal/execution"
)

func TestAddressPolicy(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::ffff:10.0.1.2", "169.254.169.254", "100.100.100.200", "::1", "fd00::1", "64:ff9b::a00:1", "192.0.0.192"} {
		if publicIP(netip.MustParseAddr(s)) {
			t.Errorf("allowed %s", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "2606:4700::1111"} {
		if !publicIP(netip.MustParseAddr(s)) {
			t.Errorf("blocked %s", s)
		}
	}
}

func TestAgentAdapter(t *testing.T) {
	var calls atomic.Int32
	text := strings.Repeat("<", 12000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("unexpected request metadata")
		}
		var req wireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		calls.Add(1)
		res := wireResponse{Version: 1, Session: req.Session, Action: req.Action}
		if req.Action == "exchange" {
			if req.Input == nil || req.Input.Message == nil || *req.Input.Message != text {
				t.Error("wrong agent input")
			}
			first, second := "first", "second"
			res.Reply = &wireReply{Response: execution.Response{Finished: true}, Messages: []*string{&first, &second}}
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	defer srv.Close()
	a := &Agents{Client: HTTPClient(4, true, true), Hosts: map[string]bool{"127.0.0.1": true}, Local: true, EnableHTTP: true, Timeout: time.Second, Limit: 64 << 10}
	defer a.Client.CloseIdleConnections()
	agent, err := a.Open(t.Context(), execution.Scope{}, execution.AgentConfig{Adapter: "http-json-v1", Endpoint: srv.URL}, " test-token ")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := agent.Exchange(t.Context(), execution.Input{Message: &text})
	if err != nil || !reply.Finished || len(reply.Messages) != 2 {
		t.Fatalf("%+v %v", reply, err)
	}
	if err := agent.Close(t.Context()); err != nil || calls.Load() != 3 {
		t.Fatalf("%v, calls=%d", err, calls.Load())
	}
}

func TestWireReplyRejectsNullElements(t *testing.T) {
	for _, input := range []string{`{"messages":[null]}`, `{"tool_calls":[null]}`} {
		var wire wireReply
		if err := execution.DecodeStrict([]byte(input), &wire); err != nil {
			t.Fatal(err)
		}
		if _, err := wire.response(); err != execution.ErrProtocol {
			t.Errorf("accepted %s: %v", input, err)
		}
	}
	var wire wireReply
	if err := execution.DecodeStrict([]byte(`{"messages":[""],"tool_calls":[]}`), &wire); err != nil {
		t.Fatal(err)
	}
	reply, err := wire.response()
	if err != nil || len(reply.Messages) != 1 || reply.Messages[0] != "" || reply.Calls == nil {
		t.Fatalf("explicit empty elements changed: %+v, %v", reply, err)
	}
}

func TestToolResultsFitWireLimit(t *testing.T) {
	const limit = 64 << 10
	agent := &httpAgent{id: uuid.NewString(), limit: limit}
	body := json.RawMessage(`{"data":"` + strings.Repeat("<", 23<<10) + `"}`)
	input := execution.Input{Results: []execution.Result{
		{ID: "one", Status: 200, Body: body}, {ID: "two", Status: 504, Body: body}, {ID: "three", Status: 409, Body: body},
	}}
	raw, err := execution.MarshalJSON(wireRequest{1, agent.id, "exchange", &input})
	if err != nil {
		t.Fatal(err)
	}
	short, err := agent.fitToolResults(input, int64(len(raw)))
	var request wireRequest
	if err != nil || len(short) > limit || json.Unmarshal(short, &request) != nil || request.Input == nil || len(request.Input.Results) != 3 {
		t.Fatalf("tool results do not fit: %d bytes, %v", len(short), err)
	}
	var omitted int
	for i, result := range request.Input.Results {
		if result.ID != input.Results[i].ID || result.Status != input.Results[i].Status || len(input.Results[i].Body) == 0 {
			t.Fatal("lost original result identity, status, or evidence")
		}
		if len(result.Body) == 0 && result.Error == "response_too_large" {
			omitted++
		}
	}
	if omitted != 1 {
		t.Fatalf("omitted %d bodies, wanted one", omitted)
	}
	agent.limit = 1024
	longIDs := make([]execution.Call, 16)
	for i := range longIDs {
		longIDs[i].ID = strings.Repeat("x", 128)
	}
	if err := agent.CheckToolResults(longIDs); err != execution.ErrMessageLimit {
		t.Fatalf("oversized metadata passed preflight: %v", err)
	}
}

func TestPCMFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"poligon.ws-pcm.v1"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		kind, b, err := conn.Read(ctx)
		var hello pcmHello
		if err != nil || kind != websocket.MessageText || json.Unmarshal(b, &hello) != nil || hello.Type != "open" {
			t.Error("invalid PCM open")
			return
		}
		hello.Type = "accepted"
		raw, _ := json.Marshal(hello)
		if err = conn.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Error(err)
			return
		}
		kind, frame, err := conn.Read(ctx)
		if err != nil || kind != websocket.MessageBinary {
			t.Error("invalid PCM frame")
			return
		}
		if err = conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
			t.Error(err)
			return
		}
		_, _, _ = conn.Read(ctx)
	}))
	defer srv.Close()
	a := &Agents{Client: HTTPClient(2, true, true), Hosts: map[string]bool{"127.0.0.1": true}, Local: true, Timeout: time.Second, Limit: 4096}
	defer a.Client.CloseIdleConnections()
	p, err := a.OpenPCM(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), "", PCMFormat{16000, 1, "s16le"}, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Write(t.Context(), []byte{1}); err == nil {
		t.Fatal("unaligned PCM accepted")
	}
	if err = p.Write(t.Context(), []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	b, err := p.Read(t.Context())
	if err != nil || len(b) != 2 || b[1] != 2 {
		t.Fatalf("invalid PCM echo: %v %v", b, err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestToolRoute(t *testing.T) {
	for _, tc := range []struct{ name, args, method, path, code string }{
		{"list_clients", "{}", "GET", "clients", ""},
		{"get_client", `{"id":"client-001"}`, "GET", "clients/client-001", ""},
		{"get_order", `{"id":"order-001"}`, "GET", "orders/order-001", ""},
		{"list_orders", `{"client_id":"client-001"}`, "GET", "orders", ""},
		{"search_slots", `{"date":"2026-11-10","available":false}`, "GET", "slots", ""},
		{"create_order", `{"client_id":"client-001","address":"main","slot_id":"slot-101"}`, "POST", "orders", ""},
		{"update_order", `{"id":"o-1","address":"main"}`, "PATCH", "orders/o-1", ""},
		{"cancel_order", `{"id":"o-1"}`, "POST", "orders/o-1/cancel", ""},
		{"cancel_order", `{"id":"../admin"}`, "", "", "invalid_arguments"},
		{"get_client", `{"id":"x","id":"y"}`, "", "", "invalid_arguments"},
		{"cancel_order", `{"id":"safe","ID":"victim"}`, "", "", "invalid_arguments"},
		{"list_orders", `{"session_id":"foreign"}`, "", "", "invalid_arguments"},
		{"reset", "{}", "", "", "unknown_tool"},
	} {
		t.Run(tc.name+"/"+tc.args, func(t *testing.T) {
			method, path, _, _, code := route(execution.Call{Name: tc.name, Arguments: json.RawMessage(tc.args)})
			if method != tc.method || path != tc.path || code != tc.code {
				t.Fatalf("%s %s %s", method, path, code)
			}
		})
	}
}

func TestEnvironmentInitializesLargeFixture(t *testing.T) {
	base := os.Getenv("POLIGON_ENVIRONMENT_TEST_URL")
	if base == "" {
		t.Skip("set POLIGON_ENVIRONMENT_TEST_URL to the pinned real Environment server")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("use a local Environment test instance")
	}
	var fixture strings.Builder
	fixture.WriteString(`{"clients":[`)
	for i := range 20000 {
		if i > 0 {
			fixture.WriteByte(',')
		}
		fmt.Fprintf(&fixture, `{"id":"client-%d"}`, i)
	}
	fixture.WriteString(`],"slots":[]}`)
	client := HTTPClient(4, true, false)
	defer client.CloseIdleConnections()
	env := &Environment{Client: client, BaseURL: base, Timeout: 5 * time.Second, Limit: 24 << 10}
	scope := execution.Scope{ProjectID: uuid.NewString(), RunID: uuid.NewString(), SessionID: uuid.NewString(), Attempt: 1}
	workspace, err := env.Open(t.Context(), scope, execution.Setup{
		Template: "field-service", TemplateVersion: "v1",
		Definition: json.RawMessage(`{"schema_version":1,"template_id":"field-service","template_version":"v1","capability_version":"field-service.v1"}`),
		Fixture:    json.RawMessage(fixture.String()), Faults: json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := workspace.Call(t.Context(), execution.Call{ID: "large-fixture", Name: "get_client", Arguments: json.RawMessage(`{"id":"client-19999"}`)})
	if result.Status != http.StatusOK || result.Error != "" {
		t.Fatalf("fixture client unavailable: %+v", result)
	}
}

func TestRealEnvironmentTimeoutAfterSave(t *testing.T) {
	base := os.Getenv("POLIGON_ENVIRONMENT_TEST_URL")
	if base == "" {
		t.Skip("set POLIGON_ENVIRONMENT_TEST_URL to the pinned real Environment server")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("use a local Environment test instance")
	}
	client := HTTPClient(4, true, false)
	defer client.CloseIdleConnections()
	env := &Environment{Client: client, BaseURL: base, Timeout: time.Second, Limit: 24 << 10}
	id := execution.Scope{ProjectID: uuid.NewString(), RunID: uuid.NewString(), SessionID: uuid.NewString(), Attempt: 1}
	setup := execution.Setup{Template: "field-service", TemplateVersion: "v1", Definition: json.RawMessage(`{"schema_version":1,"template_id":"field-service","template_version":"v1","capability_version":"field-service.v1"}`),
		Fixture: json.RawMessage(`{}`), Faults: json.RawMessage(`[{"fault_type":"timeout_after_save","method":"POST","path_pattern":"/api/v1/orders","http_status":504,"max_triggers":1}]`)}
	ws, err := env.Open(t.Context(), id, setup)
	if err != nil {
		t.Fatal(err)
	}
	call := execution.Call{ID: "test-1", Name: "create_order", IdempotencyKey: "original-key",
		Arguments: json.RawMessage(`{"client_id":"client-001","address":"Test Street 10","date":"2026-11-10","time_window":"17:00"}`)}
	first := ws.Call(t.Context(), call)
	if first.Status != 504 || !strings.Contains(string(first.Body), "timeout_after_save") {
		t.Fatalf("did not preserve fault: %+v %s", first, first.Body)
	}
	list := execution.Call{ID: "list", Name: "list_orders", Arguments: json.RawMessage(`{}`)}
	orders := func(w execution.Workspace) int {
		t.Helper()
		out := w.Call(t.Context(), list)
		var v struct {
			Count int `json:"count"`
		}
		if out.Status != 200 || json.Unmarshal(out.Body, &v) != nil {
			t.Fatalf("cannot inspect CRM: %+v", out)
		}
		return v.Count
	}
	if orders(ws) != 1 {
		t.Fatal("HTTP 504 did not commit exactly one order")
	}
	call.ID = "agent-retry"
	if next := ws.Call(t.Context(), call); next.Status != 200 || orders(ws) != 1 {
		t.Fatalf("idempotent retry failed: %+v", next)
	}
	id.Attempt++
	other, err := env.Open(t.Context(), id, setup)
	if err != nil || orders(other) != 0 {
		t.Fatalf("attempts not isolated: %v", err)
	}
}

func BenchmarkRoute(b *testing.B) {
	c := execution.Call{Name: "create_order", Arguments: json.RawMessage(`{"client_id":"client-001","address":"main","slot_id":"slot-101"}`)}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, _, _, code := route(c); code != "" {
			b.Fatal(code)
		}
	}
}

func FuzzToolRoute(f *testing.F) {
	f.Add("cancel_order", `{"id":"../admin"}`)
	f.Add("create_order", `{"client_id":"client-1","address":"test","slot_id":"slot-101"}`)
	f.Fuzz(func(t *testing.T, name, args string) {
		if len(args) > 16<<10 {
			return
		}
		method, path, _, _, code := route(execution.Call{Name: name, Arguments: json.RawMessage(args)})
		if code == "" && (method == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "/../") || strings.Contains(path, "%")) {
			t.Fatal(fmt.Sprintf("unsafe route %q %q", method, path))
		}
	})
}
