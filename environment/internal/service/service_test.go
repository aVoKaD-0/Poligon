package service

import (
	"errors"
	"sync"
	"testing"

	"poligon/environment/internal/domain"
	"poligon/environment/internal/store"
)

func TestConcurrentMovesKeepOneOwnerAndReleasePreviousSlots(t *testing.T) {
	m := store.NewManager()
	svc := NewService(m)
	order, _, err := svc.CreateOrder("s", CreateOrderRequest{ClientID: "client-001", Address: "A", SlotID: "slot-101"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, slotID := range []string{"slot-102", "slot-103"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			if _, err := svc.UpdateOrder("s", order.ID, UpdateOrderRequest{SlotID: &id}); err != nil {
				t.Error(err)
			}
		}(slotID)
	}
	close(start)
	wg.Wait()
	snapshot := m.GetOrCreate("s").GetSnapshot()
	final := snapshot.Orders[order.ID]
	reserved := 0
	for _, slot := range snapshot.Slots {
		if slot.ReservedBy == order.ID {
			reserved++
			if slot.ID != final.SlotID || slot.Date != final.Date || slot.IsAvailable {
				t.Fatalf("booking and order disagree: %+v %+v", slot, final)
			}
		}
	}
	if reserved != 1 {
		t.Fatalf("order owns %d slots, want 1", reserved)
	}
}

func TestConcurrentUpdateCannotResurrectCancelledOrder(t *testing.T) {
	for i := 0; i < 50; i++ {
		m := store.NewManager()
		svc := NewService(m)
		order, _, err := svc.CreateOrder("s", CreateOrderRequest{ClientID: "client-001", Address: "A", SlotID: "slot-101"})
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			address := "B"
			_, err := svc.UpdateOrder("s", order.ID, UpdateOrderRequest{Address: &address})
			if err != nil && !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.CancelOrder("s", order.ID); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
		snapshot := m.GetOrCreate("s").GetSnapshot()
		if snapshot.Orders[order.ID].Status != domain.OrderStatusCancelled || !snapshot.Slots["slot-101"].IsAvailable {
			t.Fatal("cancelled order was resurrected or slot was not released")
		}
	}
}

func TestConflictingMovePreservesOriginalBookingAndAudit(t *testing.T) {
	m := store.NewManager()
	svc := NewService(m)
	first, _, err := svc.CreateOrder("s", CreateOrderRequest{ClientID: "client-001", Address: "A", SlotID: "slot-101"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := svc.CreateOrder("s", CreateOrderRequest{ClientID: "client-002", Address: "B", SlotID: "slot-102"})
	if err != nil {
		t.Fatal(err)
	}
	sess := m.GetOrCreate("s")
	before := len(sess.GetAuditLog())
	_, err = svc.UpdateOrder("s", first.ID, UpdateOrderRequest{SlotID: &second.SlotID})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected slot conflict, got %v", err)
	}
	snapshot := sess.GetSnapshot()
	if snapshot.Orders[first.ID] != first || snapshot.Slots[first.SlotID].ReservedBy != first.ID || snapshot.Slots[second.SlotID].ReservedBy != second.ID || len(sess.GetAuditLog()) != before {
		t.Fatal("failed move changed state or audit")
	}
}

func TestSlotParametersValidatedOnCreateAndUpdate(t *testing.T) {
	for _, field := range []string{"date", "time_window"} {
		t.Run(field, func(t *testing.T) {
			m := store.NewManager()
			svc := NewService(m)
			req := CreateOrderRequest{ClientID: "client-001", Address: "A", SlotID: "slot-101"}
			bad := "invalid"
			if field == "date" {
				req.Date = bad
			} else {
				req.TimeWindow = bad
			}
			if _, _, err := svc.CreateOrder("s", req); !errors.Is(err, ErrBadRequest) {
				t.Fatalf("create accepted mismatching %s: %v", field, err)
			}
			if snap := m.GetOrCreate("s").GetSnapshot(); snap.TotalOrdersCount != 0 || !snap.Slots[req.SlotID].IsAvailable {
				t.Fatal("invalid create changed state")
			}
			req.Date, req.TimeWindow = "", ""
			order, _, err := svc.CreateOrder("s", req)
			if err != nil {
				t.Fatal(err)
			}
			for _, move := range []bool{false, true} {
				update := UpdateOrderRequest{}
				if field == "date" {
					update.Date = &bad
				} else {
					update.TimeWindow = &bad
				}
				newSlot := "slot-102"
				if move {
					update.SlotID = &newSlot
				}
				if _, err := svc.UpdateOrder("s", order.ID, update); !errors.Is(err, ErrBadRequest) {
					t.Fatalf("update accepted mismatching %s: %v", field, err)
				}
				if got, _ := svc.GetOrder("s", order.ID); got != order {
					t.Fatal("invalid update changed order")
				}
			}
		})
	}
}
