package faults

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"poligon/environment/internal/domain"
)

// ValidateRule rejects configurations that cannot represent a supported failure.
func ValidateRule(rule domain.FaultRule) error {
	switch rule.FaultType {
	case domain.FaultTimeoutAfterSave, domain.FaultTimeoutBeforeSave, domain.FaultDelay,
		domain.FaultConflict, domain.FaultServerError, domain.FaultRateLimit:
	default:
		return fmt.Errorf("unsupported fault_type %q", rule.FaultType)
	}
	if rule.HttpStatus != 0 && (rule.HttpStatus < 400 || rule.HttpStatus > 599) {
		return fmt.Errorf("http_status must be 0 or an error status between 400 and 599")
	}
	if rule.DelayMs < 0 || int64(rule.DelayMs) > int64((1<<63-1)/time.Millisecond) {
		return fmt.Errorf("delay_ms must be non-negative and fit in a time.Duration")
	}
	if rule.MaxTriggers < 0 {
		return fmt.Errorf("max_triggers must be non-negative")
	}
	return nil
}

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
	for _, rule := range rules {
		e.AddRule(rule)
	}
	return e
}

// AddRule registers a new fault rule.
func (e *Engine) AddRule(rule domain.FaultRule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ruleCopy := cloneRule(rule)
	ruleCopy.TriggerCount = 0
	if ruleCopy.ID == "" {
		ruleCopy.ID = "fault-" + time.Now().Format("150405.000000")
	}
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
		result[i] = cloneRule(*r)
	}
	return result
}

// Reset rearms the configured scenario, including dynamically added rules.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rule := range e.rules {
		rule.TriggerCount = 0
	}
}

func cloneRule(rule domain.FaultRule) domain.FaultRule {
	if rule.Active != nil {
		active := *rule.Active
		rule.Active = &active
	}
	return rule
}

// matchRule consumes a trigger atomically and returns an independent copy.
func (e *Engine) matchRule(method string, paths ...string) *domain.FaultRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rule := range e.rules {
		if rule.Active != nil && !*rule.Active {
			continue
		}
		if rule.MaxTriggers > 0 && rule.TriggerCount >= rule.MaxTriggers {
			continue
		}
		if !matchFilter(rule.Method, method) {
			continue
		}
		matched := false
		for _, path := range paths {
			if matchFilter(rule.PathPattern, path) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		rule.TriggerCount++
		copy := cloneRule(*rule)
		return &copy
	}
	return nil
}

// MatchPreRequest evaluates faults before execution and returns a copy of any post-save rule.
func (e *Engine) MatchPreRequest(method, path string, aliases ...string) (postSaveRule *domain.FaultRule, injectedErr *InjectedError) {
	rule := e.matchRule(method, append([]string{path}, aliases...)...)
	if rule == nil {
		return nil, nil
	}
	// Artificial delays must not hold the engine lock or block unrelated calls.
	switch rule.FaultType {
	case domain.FaultDelay:
		if rule.DelayMs > 0 {
			time.Sleep(time.Duration(rule.DelayMs) * time.Millisecond)
		}
		return nil, nil
	case domain.FaultTimeoutAfterSave:
		return rule, nil
	}
	var status int
	var message string
	switch rule.FaultType {
	case domain.FaultTimeoutBeforeSave:
		if rule.DelayMs > 0 {
			time.Sleep(time.Duration(rule.DelayMs) * time.Millisecond)
		}
		status, message = 504, "Simulated upstream timeout before processing"
	case domain.FaultConflict:
		status, message = 409, "Simulated resource conflict"
	case domain.FaultServerError:
		status, message = 500, "Simulated internal server error"
	case domain.FaultRateLimit:
		status, message = 429, "Simulated rate limit exceeded"
	default:
		return nil, nil
	}
	if rule.HttpStatus != 0 {
		status = rule.HttpStatus
	}
	if rule.ErrorMessage != "" {
		message = rule.ErrorMessage
	}
	return nil, &InjectedError{FaultType: rule.FaultType, StatusCode: status, Message: message}
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
