package faults

import (
	"testing"
	"time"

	"poligon/environment/internal/domain"
)

func TestFaultEngine_PreRequestMatching(t *testing.T) {
	rules := []domain.FaultRule{
		{
			ID:          "rule-1",
			FaultType:   domain.FaultConflict,
			Method:      "POST",
			PathPattern: "/api/v1/orders",
			HttpStatus:  409,
			MaxTriggers: 1,
			Active:      true,
		},
	}

	engine := NewEngine(rules)

	// 1st request matches rule and fails
	postSave, err := engine.MatchPreRequest("POST", "/api/v1/orders")
	if postSave != nil {
		t.Errorf("expected no postSave rule, got %v", postSave)
	}
	if err == nil || err.StatusCode != 409 {
		t.Fatalf("expected status 409, got %v", err)
	}

	// 2nd request exceeds MaxTriggers (1) so it should pass without error
	postSave2, err2 := engine.MatchPreRequest("POST", "/api/v1/orders")
	if postSave2 != nil || err2 != nil {
		t.Errorf("expected 2nd request to pass, got err=%v", err2)
	}
}

func TestFaultEngine_TimeoutAfterSave(t *testing.T) {
	rules := []domain.FaultRule{
		{
			ID:          "rule-timeout",
			FaultType:   domain.FaultTimeoutAfterSave,
			Method:      "POST",
			PathPattern: "/api/v1/orders",
			DelayMs:     5,
			HttpStatus:  504,
			MaxTriggers: 1,
			Active:      true,
		},
	}

	engine := NewEngine(rules)

	postSave, err := engine.MatchPreRequest("POST", "/api/v1/orders")
	if err != nil {
		t.Fatalf("expected no pre-request error for timeout_after_save, got %v", err)
	}
	if postSave == nil {
		t.Fatalf("expected postSave rule to be non-nil")
	}

	// Now handle post-save
	start := time.Now()
	postErr := engine.HandlePostSave(postSave)
	duration := time.Since(start)

	if postErr == nil || postErr.StatusCode != 504 {
		t.Fatalf("expected 504 status from HandlePostSave, got %v", postErr)
	}
	if duration < 5*time.Millisecond {
		t.Errorf("expected delay of at least 5ms, took %v", duration)
	}
}
