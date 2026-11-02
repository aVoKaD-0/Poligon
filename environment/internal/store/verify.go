package store

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"poligon/environment/internal/domain"
)

// Verify scopes assertions and report counts to ClientID when provided.
// Field assertions use the most recently updated active order, or the most
// recently updated order of any status when ExpectedStatus is specified.
func (s *SessionState) Verify(criteria domain.VerificationCriteria) domain.VerificationReport {
	s.mu.RLock()
	defer s.mu.RUnlock()

	report := domain.VerificationReport{
		SessionID: s.SessionID, CheckedAt: time.Now(), OverallPass: true,
		Assertions: make([]domain.AssertionResult, 0),
	}
	orders := make([]domain.Order, 0)
	activeByClient := make(map[string]int)
	for _, order := range s.Orders {
		if criteria.ClientID != "" && order.ClientID != criteria.ClientID {
			continue
		}
		report.TotalOrders++
		if order.Status == domain.OrderStatusActive {
			report.ActiveOrders++
			activeByClient[order.ClientID]++
		}
		if order.Status == domain.OrderStatusActive || criteria.ExpectedStatus != "" {
			orders = append(orders, order)
		}
	}
	sort.Slice(orders, func(i, j int) bool {
		a, b := orders[i], orders[j]
		aTime, bTime := a.UpdatedAt, b.UpdatedAt
		if aTime.IsZero() {
			aTime = a.CreatedAt
		}
		if bTime.IsZero() {
			bTime = b.CreatedAt
		}
		if !aTime.Equal(bTime) {
			return aTime.After(bTime)
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
	add := func(name string, passed bool, message string, details []string) {
		report.Assertions = append(report.Assertions, domain.AssertionResult{
			Name: name, Passed: passed, Message: message, Details: details,
		})
		report.OverallPass = report.OverallPass && passed
	}
	if criteria.ExpectedActiveOrdersCount != nil {
		expected := *criteria.ExpectedActiveOrdersCount
		details := []string{fmt.Sprintf("client_id=%s", criteria.ClientID)}
		for _, order := range orders {
			if order.Status == domain.OrderStatusActive {
				details = append(details, fmt.Sprintf("order_id=%s client_id=%s date=%s time=%s", order.ID, order.ClientID, order.Date, order.TimeWindow))
			}
		}
		add("expected_active_orders_count", report.ActiveOrders == expected,
			fmt.Sprintf("Expected %d active orders, found %d", expected, report.ActiveOrders),
			details)
	}
	if criteria.DisallowDuplicates {
		clientIDs := make([]string, 0, len(activeByClient))
		for id := range activeByClient {
			clientIDs = append(clientIDs, id)
		}
		sort.Strings(clientIDs)
		passed := true
		details := make([]string, 0, len(clientIDs))
		for _, id := range clientIDs {
			count := activeByClient[id]
			passed = passed && count <= 1
			details = append(details, fmt.Sprintf("client_id=%s active_orders=%d", id, count))
		}
		for _, order := range orders {
			if order.Status == domain.OrderStatusActive {
				details = append(details, fmt.Sprintf("order_id=%s client_id=%s date=%s time=%s address=%s", order.ID, order.ClientID, order.Date, order.TimeWindow, order.Address))
			}
		}
		add("no_duplicate_orders_for_client", passed, "At most one active order per client", details)
	}
	var latest domain.Order
	if len(orders) > 0 {
		latest = orders[0]
	}
	for _, check := range []struct {
		name, expected, actual string
	}{
		{"expected_address_match", criteria.ExpectedAddress, latest.Address},
		{"expected_time_window_match", criteria.ExpectedTimeWindow, latest.TimeWindow},
		{"expected_date_match", criteria.ExpectedDate, latest.Date},
		{"expected_status_match", criteria.ExpectedStatus, string(latest.Status)},
	} {
		if check.expected == "" {
			continue
		}
		if len(orders) == 0 {
			add(check.name, false, "No matching order exists",
				[]string{fmt.Sprintf("client_id=%s expected=%q", criteria.ClientID, check.expected)})
			continue
		}
		add(check.name, strings.TrimSpace(check.expected) == strings.TrimSpace(check.actual),
			fmt.Sprintf("Expected %q, found %q", check.expected, check.actual),
			[]string{fmt.Sprintf("order_id=%s client_id=%s", latest.ID, latest.ClientID)})
	}
	return report
}
