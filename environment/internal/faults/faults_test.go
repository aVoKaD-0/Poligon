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

func TestFaultActivationDefaultsAndExplicitDisable(t *testing.T) {
	inactive := false
	for _, addDynamically := range []bool{false, true} {
		for _, active := range []*bool{nil, &inactive} {
			rule := domain.FaultRule{FaultType: domain.FaultConflict, Active: active, MaxTriggers: 1}
			var engine *Engine
			if addDynamically {
				engine = NewEngine(nil)
				engine.AddRule(rule)
			} else {
				engine = NewEngine([]domain.FaultRule{rule})
			}
			_, err := engine.MatchPreRequest("POST", "/api/v1/orders")
			if (err != nil) != (active == nil) {
				t.Fatalf("unexpected activation: active=%v error=%v", active, err)
			}
			if active == nil {
				engine.Reset()
				if _, err := engine.MatchPreRequest("POST", "/api/v1/orders"); err == nil {
					t.Fatal("reset did not rearm rule")
				}
			}
		}
	}
}

func TestFaultRulesDoNotShareActivationPointers(t *testing.T) {
	active := true
	engine := NewEngine([]domain.FaultRule{{FaultType: domain.FaultConflict, Active: &active}})
	active = false
	returned := engine.GetRules()
	*returned[0].Active = false
	if _, err := engine.MatchPreRequest("POST", "/api/v1/orders"); err == nil {
		t.Fatal("caller changed stored rule through a shared pointer")
	}
}
