package test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"poligon/environment/internal/domain"
	"poligon/environment/internal/service"
	"poligon/environment/internal/store"
)

func requestJSON(t *testing.T, baseURL, method, path, session, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Session-ID", session)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, b
}

func TestRegressionConcurrentIdempotency(t *testing.T) {
	for round := 0; round < 20; round++ {
		m := store.NewManager()
		svc := service.NewService(m)
		m.GetOrCreate("s")
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _, err := svc.CreateOrder("s", service.CreateOrderRequest{
					ClientID: "client-001", Address: "A", Date: "2026-10-30", IdempotencyKey: "same-key",
				})
				if err != nil {
					t.Error(err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := m.GetOrCreate("s").GetSnapshot().TotalOrdersCount; got != 1 {
			t.Fatalf("same idempotency key created %d orders in round %d; want 1", got, round)
		}
	}
}

func TestRegressionConcurrentSlotReservation(t *testing.T) {
	for round := 0; round < 50; round++ {
		m := store.NewManager()
		svc := service.NewService(m)
		m.GetOrCreate("s")
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _, err := svc.CreateOrder("s", service.CreateOrderRequest{ClientID: "client-001", Address: "A", SlotID: "slot-101"})
				if err != nil && !errors.Is(err, service.ErrConflict) {
					t.Error(err)
				}
			}()
		}
		close(start)
		wg.Wait()
		snap := m.GetOrCreate("s").GetSnapshot()
		if got := snap.TotalOrdersCount; got != 1 {
			t.Fatalf("one slot has %d successful orders; slot reserved_by=%s", got, snap.Slots["slot-101"].ReservedBy)
		}
	}
}

func TestRegressionVerifyMissingOrder(t *testing.T) {
	sess := store.NewManager().GetOrCreate("s")
	report := sess.Verify(domain.VerificationCriteria{ClientID: "client-001", ExpectedAddress: "A", ExpectedTimeWindow: "18:00-19:00"})
	if report.OverallPass {
		t.Fatalf("empty CRM passed requested address/time assertions: %+v", report)
	}
}

func TestRegressionVerifyClientScope(t *testing.T) {
	sess := store.NewManager().GetOrCreate("s")
	sess.SaveOrder(domain.Order{ID: "other", ClientID: "client-002", Address: "A", Status: domain.OrderStatusActive})
	one := 1
	report := sess.Verify(domain.VerificationCriteria{ClientID: "client-001", ExpectedActiveOrdersCount: &one, DisallowDuplicates: true, ExpectedAddress: "A"})
	if report.OverallPass {
		t.Fatalf("another client's order passed target client's criteria: %+v", report)
	}
}

func TestRegressionVerifyLatestOrder(t *testing.T) {
	sess := store.NewManager().GetOrCreate("s")
	sess.SaveOrder(domain.Order{ID: "old", ClientID: "client-001", Address: "old", CreatedAt: time.Unix(1, 0), Status: domain.OrderStatusActive})
	sess.SaveOrder(domain.Order{ID: "new", ClientID: "client-001", Address: "new", CreatedAt: time.Unix(2, 0), Status: domain.OrderStatusActive})
	passed, failed := 0, 0
	for i := 0; i < 200; i++ {
		report := sess.Verify(domain.VerificationCriteria{ClientID: "client-001", ExpectedAddress: "new"})
		if report.OverallPass {
			passed++
		} else {
			failed++
		}
	}
	if failed > 0 {
		t.Fatalf("unchanged CRM produced %d passes and %d failures for newest order", passed, failed)
	}
}

func TestRegressionVerifyDateAndStatus(t *testing.T) {
	sess := store.NewManager().GetOrCreate("s")
	sess.SaveOrder(domain.Order{ID: "o", ClientID: "client-001", Date: "2026-10-30", Status: domain.OrderStatusActive})
	for name, criteria := range map[string]domain.VerificationCriteria{
		"date":   {ClientID: "client-001", ExpectedDate: "2026-11-02"},
		"status": {ClientID: "client-001", ExpectedStatus: "cancelled"},
	} {
		t.Run(name, func(t *testing.T) {
			report := sess.Verify(criteria)
			if report.OverallPass {
				t.Fatalf("mismatching %s passed with %d assertions", name, len(report.Assertions))
			}
		})
	}
}

func TestRegressionInitDefaultFixtures(t *testing.T) {
	server, _ := setupTestServer()
	defer server.Close()
	status, b := requestJSON(t, server.URL, "POST", "/api/v1/sessions", "s", `{"session_id":"s"}`)
	var res struct {
		Snapshot domain.StateSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if status != 201 || len(res.Snapshot.Clients) == 0 || len(res.Snapshot.Slots) == 0 {
		t.Fatalf("init default fixtures: status=%d clients=%d slots=%d", status, len(res.Snapshot.Clients), len(res.Snapshot.Slots))
	}
}

func TestRegressionFaultFromInit(t *testing.T) {
	server, _ := setupTestServer()
	defer server.Close()
	requestJSON(t, server.URL, "POST", "/api/v1/sessions", "s", `{"session_id":"s","fault_rules":[{"fault_type":"timeout_after_save","method":"POST","path_pattern":"/api/v1/orders","max_triggers":1}]}`)
	status, b := requestJSON(t, server.URL, "POST", "/api/v1/orders", "s", `{"client_id":"client-001","address":"A","date":"2026-10-30"}`)
	if status != 504 {
		t.Fatalf("fault supplied during init returned %d instead of 504: %s", status, b)
	}
}

func TestRegressionResetFaultCounters(t *testing.T) {
	server, m := setupTestServer()
	defer server.Close()
	m.GetOrCreate("s").Faults().AddRule(domain.FaultRule{FaultType: domain.FaultTimeoutAfterSave, Method: "POST", PathPattern: "/api/v1/orders", MaxTriggers: 1})
	body := `{"client_id":"client-001","address":"A","date":"2026-10-30"}`
	first, _ := requestJSON(t, server.URL, "POST", "/api/v1/orders", "s", body)
	if first != 504 {
		t.Fatalf("setup: first response %d", first)
	}
	requestJSON(t, server.URL, "POST", "/api/v1/sessions/s/reset", "s", "")
	second, b := requestJSON(t, server.URL, "POST", "/api/v1/orders", "s", body)
	if second != 504 {
		t.Fatalf("same scenario after reset returned %d instead of 504: %s", second, b)
	}
}

func TestRegressionMalformedInitLosesState(t *testing.T) {
	for _, body := range []string{`{"session_id":"s","clients":`, `{"session_id":"s"} {}`, `{"session_id":"s"} garbage`, `null`, `[]`, ``} {
		t.Run(body, func(t *testing.T) {
			server, m := setupTestServer()
			defer server.Close()
			sess := m.GetOrCreate("s")
			sess.SaveOrder(domain.Order{ID: "original", Status: domain.OrderStatusActive})
			status, b := requestJSON(t, server.URL, "POST", "/api/v1/sessions", "s", body)
			remaining := m.GetOrCreate("s").GetSnapshot().TotalOrdersCount
			if status != 400 || remaining != 1 {
				t.Fatalf("invalid JSON returned %d and left %d orders (want 400 and 1): %s", status, remaining, b)
			}
		})
	}
}

func TestRegressionPostSaveFaultOnUpdateAndCancel(t *testing.T) {
	for _, operation := range []string{"update", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			server, m := setupTestServer()
			defer server.Close()
			sess := m.GetOrCreate("s")
			sess.SaveOrder(domain.Order{ID: "o", ClientID: "client-001", Address: "A", Status: domain.OrderStatusActive})
			method, path, body := "PATCH", "/api/v1/orders/o", `{"address":"B"}`
			if operation == "cancel" {
				method, path, body = "POST", "/api/v1/orders/o/cancel", ""
			}
			sess.Faults().AddRule(domain.FaultRule{FaultType: domain.FaultTimeoutAfterSave, Method: method, PathPattern: path, MaxTriggers: 1})
			status, b := requestJSON(t, server.URL, method, path, "s", body)
			if status != 504 {
				t.Fatalf("post-save fault on %s returned %d, trigger count=%d: %s", operation, status, sess.Faults().GetRules()[0].TriggerCount, b)
			}
			order, _ := sess.GetOrder("o")
			if operation == "update" && order.Address != "B" {
				t.Fatal("update was not committed before timeout")
			}
			if operation == "cancel" && order.Status != domain.OrderStatusCancelled {
				t.Fatal("cancel was not committed before timeout")
			}
			events := sess.GetAuditLog()
			last := events[len(events)-1]
			if last.ResponseStatus != 504 || last.FaultInjected != "timeout_after_save" || last.EntityID != "o" {
				t.Fatalf("missing post-save audit evidence: %+v", last)
			}
		})
	}
}

func TestRegressionSlotAndOrderDateConsistency(t *testing.T) {
	m := store.NewManager()
	svc := service.NewService(m)
	ord, _, err := svc.CreateOrder("s", service.CreateOrderRequest{ClientID: "client-001", Address: "A", SlotID: "slot-101", Date: "2099-01-01", TimeWindow: "10:00-11:00"})
	if errors.Is(err, service.ErrBadRequest) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	slot, _ := m.GetOrCreate("s").GetSlot(ord.SlotID)
	if ord.Date != slot.Date || ord.TimeWindow != fmt.Sprintf("%s-%s", slot.StartTime, slot.EndTime) {
		t.Fatalf("accepted inconsistent booking: order date=%s time=%s, reserved slot date=%s time=%s-%s", ord.Date, ord.TimeWindow, slot.Date, slot.StartTime, slot.EndTime)
	}
}
