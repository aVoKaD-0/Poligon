package faults

import (
	"strings"
	"sync"
	"time"

	"poligon/environment/internal/domain"
)

// InjectedError represents an error triggered by the fault injection engine.
type InjectedError struct {
	FaultType  domain.FaultType
	StatusCode int
	Message    string
}

func (e *InjectedError) Error() string {
	return e.Message
}

// Engine manages fault rules and evaluates whether an incoming request should trigger a simulated failure.
type Engine struct {
	mu    sync.RWMutex
	rules []*domain.FaultRule
}

// NewEngine creates a new fault injection engine.
func NewEngine(rules []domain.FaultRule) *Engine {
	e := &Engine{
		rules: make([]*domain.FaultRule, 0, len(rules)),
	}
	for _, r := range rules {
		ruleCopy := r
		e.rules = append(e.rules, &ruleCopy)
	}
	return e
}

// AddRule registers a new fault rule.
func (e *Engine) AddRule(rule domain.FaultRule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ruleCopy := rule
	if ruleCopy.ID == "" {
		ruleCopy.ID = "fault-" + time.Now().Format("150405.000000")
	}
	ruleCopy.Active = true
	e.rules = append(e.rules, &ruleCopy)
}

// ClearRules removes all fault rules.
func (e *Engine) ClearRules() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = []*domain.FaultRule{}
}

// GetRules returns a copy of current rules.
func (e *Engine) GetRules() []domain.FaultRule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make([]domain.FaultRule, len(e.rules))
	for i, r := range e.rules {
		result[i] = *r
	}
	return result
}

// MatchPreRequest checks if a rule triggers before request execution.
// Returns an InjectedError if the request should fail immediately, or postSaveRule if timeout_after_save applies.
func (e *Engine) MatchPreRequest(method, path string) (postSaveRule *domain.FaultRule, injectedErr *InjectedError) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, rule := range e.rules {
		if !rule.Active {
			continue
		}
		if rule.MaxTriggers > 0 && rule.TriggerCount >= rule.MaxTriggers {
			continue
		}
		if !matchFilter(rule.Method, method) || !matchFilter(rule.PathPattern, path) {
			continue
		}

		// Matched rule! Increment trigger count.
		rule.TriggerCount++

		switch rule.FaultType {
		case domain.FaultDelay:
			if rule.DelayMs > 0 {
				time.Sleep(time.Duration(rule.DelayMs) * time.Millisecond)
			}
			return nil, nil

		case domain.FaultTimeoutAfterSave:
			// Let the save operation complete first, then timeout!
			return rule, nil

		case domain.FaultTimeoutBeforeSave:
			if rule.DelayMs > 0 {
				time.Sleep(time.Duration(rule.DelayMs) * time.Millisecond)
			}
			status := rule.HttpStatus
			if status == 0 {
				status = 504
			}
			msg := rule.ErrorMessage
			if msg == "" {
				msg = "Simulated upstream timeout before processing"
			}
			return nil, &InjectedError{
				FaultType:  domain.FaultTimeoutBeforeSave,
				StatusCode: status,
				Message:    msg,
			}

		case domain.FaultConflict:
			status := rule.HttpStatus
			if status == 0 {
				status = 409
			}
			msg := rule.ErrorMessage
			if msg == "" {
				msg = "Simulated resource conflict"
			}
			return nil, &InjectedError{
				FaultType:  domain.FaultConflict,
				StatusCode: status,
				Message:    msg,
			}

		case domain.FaultServerError:
			status := rule.HttpStatus
			if status == 0 {
				status = 500
			}
			msg := rule.ErrorMessage
			if msg == "" {
				msg = "Simulated internal server error"
			}
			return nil, &InjectedError{
				FaultType:  domain.FaultServerError,
				StatusCode: status,
				Message:    msg,
			}

		case domain.FaultRateLimit:
			status := rule.HttpStatus
			if status == 0 {
				status = 429
			}
			msg := rule.ErrorMessage
			if msg == "" {
				msg = "Simulated rate limit exceeded"
			}
			return nil, &InjectedError{
				FaultType:  domain.FaultRateLimit,
				StatusCode: status,
				Message:    msg,
			}
		}
	}

	return nil, nil
}

// HandlePostSave executes post-mutation timeout for timeout_after_save.
func (e *Engine) HandlePostSave(rule *domain.FaultRule) *InjectedError {
	if rule == nil {
		return nil
	}
	if rule.DelayMs > 0 {
		time.Sleep(time.Duration(rule.DelayMs) * time.Millisecond)
	}
	status := rule.HttpStatus
	if status == 0 {
		status = 504
	}
	msg := rule.ErrorMessage
	if msg == "" {
		msg = "Simulated network timeout: operation saved in CRM but response failed to deliver"
	}
	return &InjectedError{
		FaultType:  domain.FaultTimeoutAfterSave,
		StatusCode: status,
		Message:    msg,
	}
}

func matchFilter(pattern, actual string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if strings.EqualFold(pattern, actual) {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(actual, prefix)
	}
	return false
}
