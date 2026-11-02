package store

import (
	"reflect"
	"testing"
	"time"

	"poligon/environment/internal/domain"
)

func TestVerifyLatestUpdateAndStableTieBreak(t *testing.T) {
	sess := NewManager().GetOrCreate("s")
	old := domain.Order{ID: "a", ClientID: "c", Address: "updated", CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(3, 0), Status: domain.OrderStatusActive}
	newer := domain.Order{ID: "b", ClientID: "c", Address: "newer", CreatedAt: time.Unix(2, 0), UpdatedAt: time.Unix(2, 0), Status: domain.OrderStatusActive}
	sess.SaveOrder(old)
	sess.SaveOrder(newer)
	criteria := domain.VerificationCriteria{ClientID: "c", ExpectedAddress: "updated"}
	if report := sess.Verify(criteria); !report.OverallPass {
		t.Fatalf("latest update was not selected: %+v", report)
	}
	// All timestamps equal: use a stable ID tie-break for identical conditions.
	old.CreatedAt, old.UpdatedAt = newer.CreatedAt, newer.UpdatedAt
	sess.SaveOrder(old)
	criteria.ExpectedAddress = "newer"
	first := sess.Verify(criteria)
	if !first.OverallPass {
		t.Fatal("tie-break selected an unexpected order")
	}
	for i := 0; i < 100; i++ {
		if got := sess.Verify(criteria); !reflect.DeepEqual(first.Assertions, got.Assertions) {
			t.Fatal("identical state returned different evidence")
		}
	}
}

func TestVerifyCancelledOrderAndClientScopedCounts(t *testing.T) {
	sess := NewManager().GetOrCreate("s")
	sess.SaveOrder(domain.Order{ID: "other", ClientID: "other", Status: domain.OrderStatusActive})
	sess.SaveOrder(domain.Order{ID: "target", ClientID: "c", Status: domain.OrderStatusCancelled, Address: "A", Date: "2026-10-30", TimeWindow: "18:00-19:00"})
	zero := 0
	report := sess.Verify(domain.VerificationCriteria{
		ClientID: "c", ExpectedActiveOrdersCount: &zero, ExpectedStatus: "cancelled",
		ExpectedAddress: "A", ExpectedDate: "2026-10-30", ExpectedTimeWindow: "18:00-19:00",
	})
	if !report.OverallPass || report.ActiveOrders != 0 || report.TotalOrders != 1 || len(report.Assertions) != 5 {
		t.Fatalf("cancelled order verification failed: %+v", report)
	}
}

func TestVerifyDuplicatesWithoutClientAndExactTimeWindow(t *testing.T) {
	sess := NewManager().GetOrCreate("s")
	for _, id := range []string{"a", "b"} {
		sess.SaveOrder(domain.Order{ID: id, ClientID: "c", Status: domain.OrderStatusActive, TimeWindow: "18:00-19:00"})
	}
	if sess.Verify(domain.VerificationCriteria{DisallowDuplicates: true}).OverallPass {
		t.Fatal("duplicate check without client ID skipped existing duplicates")
	}
	if sess.Verify(domain.VerificationCriteria{ClientID: "c", ExpectedTimeWindow: "18:00"}).OverallPass {
		t.Fatal("partial time window accepted as an exact match")
	}
}
