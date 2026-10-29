package store

import (
	"sync"
	"testing"

	"poligon/environment/internal/domain"
)

func TestStore_ConcurrentAccess(t *testing.T) {
	manager := NewManager()
	sess := manager.GetOrCreate("test-concurrency")

	var wg sync.WaitGroup
	// Concurrently read and write
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(idx int) {
			defer wg.Done()
			sess.SaveOrder(domain.Order{
				ID:        "ord-conc",
				Status:    domain.OrderStatusActive,
				Address:   "ул. Тестовая",
				SessionID: "test-concurrency",
			})
		}(i)

		go func() {
			defer wg.Done()
			_ = sess.GetSnapshot()
		}()
	}
	wg.Wait()

	snap := sess.GetSnapshot()
	if snap.TotalOrdersCount != 1 {
		t.Errorf("expected 1 order, got %d", snap.TotalOrdersCount)
	}
}

func TestStore_VerificationCriteria(t *testing.T) {
	manager := NewManager()
	sess := manager.GetOrCreate("test-verify")

	sess.SaveOrder(domain.Order{
		ID:         "ord-1",
		ClientID:   "client-1",
		Address:    "ул. Ленина, 10",
		Date:       "2026-10-30",
		TimeWindow: "после 18:00",
		Status:     domain.OrderStatusActive,
	})

	sess.SaveOrder(domain.Order{
		ID:         "ord-2",
		ClientID:   "client-1",
		Address:    "ул. Ленина, 10",
		Date:       "2026-10-30",
		TimeWindow: "после 18:00",
		Status:     domain.OrderStatusActive,
	})

	// Case 1: Detect duplicate for client-1
	report := sess.Verify(domain.VerificationCriteria{
		ClientID:           "client-1",
		DisallowDuplicates: true,
	})
	if report.OverallPass {
		t.Errorf("expected duplicate check to fail when 2 orders exist")
	}

	// Case 2: Expected active orders count
	expected := 2
	reportCount := sess.Verify(domain.VerificationCriteria{
		ExpectedActiveOrdersCount: &expected,
	})
	if !reportCount.OverallPass {
		t.Errorf("expected count check of 2 to pass")
	}
}
