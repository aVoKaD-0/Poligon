package test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"poligon/environment/internal/domain"
	"poligon/environment/internal/service"
	"poligon/environment/internal/store"
	"poligon/environment/pkg/api"
	"poligon/environment/pkg/client"
)

func TestSelfCheckSessionPrefixedFaults(t *testing.T) {
	for _, operation := range []string{"create", "update", "cancel"} {
		for _, patternKind := range []string{"canonical", "explicit", "wildcard"} {
			t.Run(operation+"/"+patternKind, func(t *testing.T) {
				server, m := setupTestServer()
				defer server.Close()
				sess := m.GetOrCreate("s")
				method, path, body := "POST", "/api/v1/orders", `{"client_id":"client-001","address":"A","date":"2026-10-30"}`
				if operation != "create" {
					sess.SaveOrder(domain.Order{ID: "o", Status: domain.OrderStatusActive, Address: "A"})
					method, path, body = "PATCH", "/api/v1/orders/o", `{"address":"B"}`
					if operation == "cancel" {
						method, path, body = "POST", "/api/v1/orders/o/cancel", ""
					}
				}
				requestPath := "/api/v1/sessions/s" + path[len("/api/v1"):]
				pattern := path
				if patternKind == "explicit" {
					pattern = requestPath
				}
				if patternKind == "wildcard" {
					pattern = "*"
				}
				sess.Faults().AddRule(domain.FaultRule{FaultType: domain.FaultTimeoutAfterSave, Method: method, PathPattern: pattern, MaxTriggers: 1})
				status, b := requestJSON(t, server.URL, method, requestPath, "", body)
				if status != 504 {
					t.Fatalf("session-prefixed route bypassed fault: %d %s", status, b)
				}
				snapshot := sess.GetSnapshot()
				if snapshot.TotalOrdersCount != 1 {
					t.Fatalf("timeout did not persist exactly one order: %d", snapshot.TotalOrdersCount)
				}
				if operation == "update" && snapshot.Orders["o"].Address != "B" {
					t.Fatal("update did not commit")
				}
				if operation == "cancel" && snapshot.Orders["o"].Status != domain.OrderStatusCancelled {
					t.Fatal("cancel did not commit")
				}
				if sess.Faults().GetRules()[0].TriggerCount != 1 {
					t.Fatal("one request consumed multiple triggers")
				}
				retry, _ := requestJSON(t, server.URL, method, requestPath, "", body)
				if retry != 200 && retry != 201 {
					t.Fatalf("fault triggered twice: status=%d", retry)
				}
			})
		}
	}
}

func TestSelfCheckAuditPayloadImmutable(t *testing.T) {
	m := store.NewManager()
	svc := service.NewService(m)
	order, _, err := svc.CreateOrder("s", service.CreateOrderRequest{ClientID: "client-001", Address: "A", Date: "2026-10-30"})
	if err != nil {
		t.Fatal(err)
	}
	address := "B"
	if _, err := svc.UpdateOrder("s", order.ID, service.UpdateOrderRequest{Address: &address}); err != nil {
		t.Fatal(err)
	}
	sess := m.GetOrCreate("s")
	events := sess.GetAuditLog()
	index := len(events) - 1
	expected, _ := json.Marshal(events[index].RequestPayload)
	address = "C"
	actual, _ := json.Marshal(sess.GetAuditLog()[index].RequestPayload)
	if !bytes.Equal(expected, actual) {
		t.Errorf("caller changed stored audit: expected %s got %s", expected, actual)
	}
	// A reader of the audit must not gain a mutable reference to its contents.
	switch value := events[index].RequestPayload.(type) {
	case api.UpdateOrderRequest:
		*value.Address = "D"
	case json.RawMessage:
		for i := range value {
			value[i] = ' '
		}
	case map[string]any:
		value["address"] = "D"
	}
	actual, _ = json.Marshal(sess.GetAuditLog()[index].RequestPayload)
	if !bytes.Equal(expected, actual) {
		t.Errorf("audit reader changed stored event: expected %s got %s", expected, actual)
	}
}

func TestSelfCheckSDKDefaultSession(t *testing.T) {
	for _, baseSuffix := range []string{"", "/"} {
		for _, sessionID := range []string{"", "  ", "nested/session?query#fragment"} {
			t.Run(baseSuffix+sessionID, func(t *testing.T) {
				server, _ := setupTestServer()
				defer server.Close()
				c := client.NewClient(server.URL+baseSuffix, sessionID)
				ctx := context.Background()
				if _, _, err := c.CreateOrder(ctx, api.CreateOrderRequest{ClientID: "client-001", Address: "A", Date: "2026-10-30"}, ""); err != nil {
					t.Fatal(err)
				}
				state, err := c.GetState(ctx)
				if err != nil {
					t.Fatal(err)
				}
				expected := "default"
				if sessionID == "nested/session?query#fragment" {
					expected = sessionID
				}
				if state.SessionID != expected || state.TotalOrdersCount != 1 {
					t.Fatalf("wrong session: %+v", state)
				}
				c.SetSessionID("")
				if _, err := c.GetAuditLog(ctx); err != nil {
					t.Fatal(err)
				}
				if err := c.AddFaultRule(ctx, api.FaultRule{FaultType: api.FaultConflict}); err != nil {
					t.Fatal(err)
				}
				if err := c.ResetSession(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Verify(ctx, api.VerificationCriteria{DisallowDuplicates: true}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSelfCheckInvalidFaultConfigPreservesSession(t *testing.T) {
	for _, rule := range []string{
		`{"fault_type":"server_error","http_status":99}`,
		`{"fault_type":"server_error","http_status":200}`,
		`{"fault_type":"unknown"}`,
		`{"fault_type":"delay","delay_ms":-1}`,
		`{"fault_type":"conflict","max_triggers":-1}`,
	} {
		for _, init := range []bool{false, true} {
			t.Run(rule+map[bool]string{false: "/add", true: "/init"}[init], func(t *testing.T) {
				server, m := setupTestServer()
				defer server.Close()
				sess := m.GetOrCreate("s")
				sess.SaveOrder(domain.Order{ID: "existing", Status: domain.OrderStatusActive})
				path, body := "/api/v1/sessions/s/faults", rule
				if init {
					path, body = "/api/v1/sessions", `{"session_id":"s","fault_rules":[`+rule+`]}`
				}
				status, b := requestJSON(t, server.URL, "POST", path, "s", body)
				if status != 400 {
					t.Errorf("invalid fault accepted: status=%d body=%s", status, b)
				}
				if m.GetOrCreate("s").GetSnapshot().TotalOrdersCount != 1 {
					t.Error("invalid config erased existing session")
				}
				if len(m.GetOrCreate("s").Faults().GetRules()) != 0 {
					t.Error("invalid config changed fault rules")
				}
			})
		}
	}
}
