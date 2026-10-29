package test

import (
	"context"
	"net/http/httptest"
	"testing"

	"poligon/environment/internal/domain"
	"poligon/environment/internal/handler"
	"poligon/environment/internal/service"
	"poligon/environment/internal/store"
	"poligon/environment/pkg/client"
)

func setupTestServer() (*httptest.Server, *store.Manager) {
	manager := store.NewManager()
	svc := service.NewService(manager)
	h := handler.NewHandler(manager, svc)
	router := handler.NewRouter(h)
	server := httptest.NewServer(router)
	return server, manager
}

func TestSessionIsolation(t *testing.T) {
	server, _ := setupTestServer()
	defer server.Close()

	ctx := context.Background()

	clientA := client.NewClient(server.URL, "session-a")
	clientB := client.NewClient(server.URL, "session-b")

	// Session A creates an order
	orderA, status, err := clientA.CreateOrder(ctx, service.CreateOrderRequest{
		ClientID:   "client-001",
		Address:    "ул. Пушкина, д. 10",
		Date:       "2026-10-30",
		TimeWindow: "после 18:00",
	}, "")
	if err != nil || status != 201 {
		t.Fatalf("session A failed to create order: %v, status %d", err, status)
	}

	// Session B checks state
	snapB, err := clientB.GetState(ctx)
	if err != nil {
		t.Fatalf("session B failed to get state: %v", err)
	}
	if snapB.ActiveOrdersCount != 0 {
		t.Errorf("expected session B to have 0 orders, got %d", snapB.ActiveOrdersCount)
	}

	// Session A checks state
	snapA, err := clientA.GetState(ctx)
	if err != nil {
		t.Fatalf("session A failed to get state: %v", err)
	}
	if snapA.ActiveOrdersCount != 1 {
		t.Errorf("expected session A to have 1 order, got %d", snapA.ActiveOrdersCount)
	}
	if _, ok := snapA.Orders[orderA.ID]; !ok {
		t.Errorf("order %s not found in session A state", orderA.ID)
	}
}

func TestOrderLifecycleAndSlotReservation(t *testing.T) {
	server, _ := setupTestServer()
	defer server.Close()

	ctx := context.Background()
	c := client.NewClient(server.URL, "session-lifecycle")

	// 1. Search available slots
	slots, err := c.SearchSlots(ctx, "2026-10-30", "выездное обслуживание")
	if err != nil {
		t.Fatalf("search slots failed: %v", err)
	}
	if len(slots) == 0 {
		t.Fatalf("expected at least 1 slot available")
	}
	targetSlot := slots[0]

	// 2. Book order with this slot
	order, status, err := c.CreateOrder(ctx, service.CreateOrderRequest{
		ClientID:   "client-001",
		SlotID:     targetSlot.ID,
		Address:    "ул. Садовая, 15",
		TimeWindow: "18:00-19:00",
	}, "")
	if err != nil || status != 201 {
		t.Fatalf("create order failed: %v, status %d", err, status)
	}

	// Verify slot is now reserved
	slotsAfter, err := c.SearchSlots(ctx, "2026-10-30", "выездное обслуживание")
	if err != nil {
		t.Fatalf("search slots after reservation failed: %v", err)
	}
	for _, s := range slotsAfter {
		if s.ID == targetSlot.ID {
			t.Errorf("expected slot %s to be reserved and not in available slots", targetSlot.ID)
		}
	}

	// 3. Update / Reschedule order
	newAddress := "ул. Новая, д. 22"
	updatedOrder, err := c.UpdateOrder(ctx, order.ID, service.UpdateOrderRequest{
		Address: &newAddress,
	})
	if err != nil {
		t.Fatalf("update order failed: %v", err)
	}
	if updatedOrder.Address != newAddress {
		t.Errorf("expected address '%s', got '%s'", newAddress, updatedOrder.Address)
	}

	// 4. Cancel order
	cancelledOrder, err := c.CancelOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("cancel order failed: %v", err)
	}
	if cancelledOrder.Status != domain.OrderStatusCancelled {
		t.Errorf("expected status cancelled, got %s", cancelledOrder.Status)
	}

	// Verify slot is released and available again
	slotsReleased, err := c.SearchSlots(ctx, "2026-10-30", "выездное обслуживание")
	if err != nil {
		t.Fatalf("search slots after cancel failed: %v", err)
	}
	foundReleased := false
	for _, s := range slotsReleased {
		if s.ID == targetSlot.ID && s.IsAvailable {
			foundReleased = true
			break
		}
	}
	if !foundReleased {
		t.Errorf("expected slot %s to be released and available again", targetSlot.ID)
	}
}

// TestCanonicalTimeoutAfterSaveScenario tests the core scenario from text.pdf:
// "Сценарий «тайм-аут после сохранения»: тестовый API создаёт заявку, но не возвращает
// успешный ответ. Стенд фиксирует факт сохранения и проверяет, не создал ли агент дубликат."
func TestCanonicalTimeoutAfterSaveScenario(t *testing.T) {
	server, _ := setupTestServer()
	defer server.Close()

	ctx := context.Background()

	// -------------------------------------------------------------
	// SCENARIO PART 1: Faulty agent without idempotency creates duplicate
	// -------------------------------------------------------------
	cNaive := client.NewClient(server.URL, "session-naive-agent")

	// Inject timeout_after_save on 1st POST to /api/v1/orders
	err := cNaive.AddFaultRule(ctx, domain.FaultRule{
		FaultType:   domain.FaultTimeoutAfterSave,
		Method:      "POST",
		PathPattern: "/api/v1/orders",
		HttpStatus:  504,
		MaxTriggers: 1, // trigger once, subsequent call succeeds
	})
	if err != nil {
		t.Fatalf("failed to add fault rule: %v", err)
	}

	// Naive agent tries to create order without idempotency key
	orderReq := service.CreateOrderRequest{
		ClientID:   "client-001",
		Address:    "ул. Тверская, д. 12, кв. 34",
		Date:       "2026-10-30",
		TimeWindow: "после 18:00",
	}

	_, status1, err1 := cNaive.CreateOrder(ctx, orderReq, "")
	if err1 == nil || status1 != 504 {
		t.Fatalf("expected HTTP 504 timeout on 1st call, got status %d, err %v", status1, err1)
	}

	// Naive agent retries after timeout without idempotency key
	_, status2, err2 := cNaive.CreateOrder(ctx, orderReq, "")
	if err2 != nil || status2 != 201 {
		t.Fatalf("expected HTTP 201 on retry, got status %d, err %v", status2, err2)
	}

	// Verify testbench detects DUPLICATE error!
	one := 1
	reportNaive, _ := cNaive.Verify(ctx, domain.VerificationCriteria{
		ExpectedActiveOrdersCount: &one,
		ClientID:                  "client-001",
		DisallowDuplicates:        true,
	})

	if reportNaive.OverallPass {
		t.Errorf("expected verification to FAIL for naive agent due to duplicate orders, but it passed!")
	}
	if reportNaive.ActiveOrders != 2 {
		t.Errorf("expected 2 active duplicate orders in CRM, got %d", reportNaive.ActiveOrders)
	}

	// Check audit log: proves order was saved during timeout!
	auditNaive, err := cNaive.GetAuditLog(ctx)
	if err != nil {
		t.Fatalf("get audit log failed: %v", err)
	}
	foundTimeoutSavedEvent := false
	for _, ev := range auditNaive {
		if ev.FaultInjected == string(domain.FaultTimeoutAfterSave) && ev.ResponseStatus == 504 && ev.EntityID != "" {
			foundTimeoutSavedEvent = true
			break
		}
	}
	if !foundTimeoutSavedEvent {
		t.Errorf("expected audit event recording timeout_after_save with entity saved")
	}

	// -------------------------------------------------------------
	// SCENARIO PART 2: Smart/Fixed agent with idempotency avoids duplicate
	// -------------------------------------------------------------
	cSmart := client.NewClient(server.URL, "session-smart-agent")

	err = cSmart.AddFaultRule(ctx, domain.FaultRule{
		FaultType:   domain.FaultTimeoutAfterSave,
		Method:      "POST",
		PathPattern: "/api/v1/orders",
		HttpStatus:  504,
		MaxTriggers: 1,
	})
	if err != nil {
		t.Fatalf("failed to add fault rule for smart agent: %v", err)
	}

	idempotencyKey := "req-idempotent-uuid-999"

	// 1st call with idempotency key -> times out after save
	_, smartStatus1, smartErr1 := cSmart.CreateOrder(ctx, orderReq, idempotencyKey)
	if smartErr1 == nil || smartStatus1 != 504 {
		t.Fatalf("expected HTTP 504 timeout on 1st smart call, got %d, %v", smartStatus1, smartErr1)
	}

	// Smart agent retries using the SAME idempotency key
	order2, smartStatus2, smartErr2 := cSmart.CreateOrder(ctx, orderReq, idempotencyKey)
	if smartErr2 != nil || (smartStatus2 != 200 && smartStatus2 != 201) {
		t.Fatalf("expected success on idempotent retry, got status %d, err %v", smartStatus2, smartErr2)
	}
	if order2 == nil {
		t.Fatalf("expected non-nil order on idempotent retry")
	}

	// Verify testbench verifies exactly 1 active order exists!
	reportSmart, err := cSmart.Verify(ctx, domain.VerificationCriteria{
		ExpectedActiveOrdersCount: &one,
		ClientID:                  "client-001",
		DisallowDuplicates:        true,
		ExpectedAddress:           "ул. Тверская, д. 12, кв. 34",
	})
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	if !reportSmart.OverallPass {
		t.Errorf("expected verification to PASS for smart agent, but it failed: %+v", reportSmart.Assertions)
	}
	if reportSmart.ActiveOrders != 1 {
		t.Errorf("expected exactly 1 active order, got %d", reportSmart.ActiveOrders)
	}
}

func TestResetState(t *testing.T) {
	server, _ := setupTestServer()
	defer server.Close()

	ctx := context.Background()
	c := client.NewClient(server.URL, "session-reset")

	// Create an order
	_, _, err := c.CreateOrder(ctx, service.CreateOrderRequest{
		ClientID:   "client-001",
		Address:    "ул. Мира, 1",
		Date:       "2026-10-31",
		TimeWindow: "10:00-12:00",
	}, "")
	if err != nil {
		t.Fatalf("create order failed: %v", err)
	}

	snapBeforeReset, err := c.GetState(ctx)
	if err != nil || snapBeforeReset.ActiveOrdersCount != 1 {
		t.Fatalf("expected 1 active order before reset, got %d", snapBeforeReset.ActiveOrdersCount)
	}

	// Reset session
	if err := c.ResetSession(ctx); err != nil {
		t.Fatalf("reset session failed: %v", err)
	}

	// Check state after reset
	snapAfterReset, err := c.GetState(ctx)
	if err != nil {
		t.Fatalf("get state after reset failed: %v", err)
	}
	if snapAfterReset.ActiveOrdersCount != 0 {
		t.Errorf("expected 0 active orders after reset, got %d", snapAfterReset.ActiveOrdersCount)
	}
}
