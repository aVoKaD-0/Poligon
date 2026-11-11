package api

import (
	"time"
)

// OrderStatus defines the status of a service appointment/order.
type OrderStatus string

const (
	OrderStatusActive    OrderStatus = "active"
	OrderStatusCancelled OrderStatus = "cancelled"
	OrderStatusCompleted OrderStatus = "completed"
)

// Client represents a customer in the CRM.
type Client struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Phone     string    `json:"phone"`
	Email     string    `json:"email,omitempty"`
	Address   string    `json:"address"`
	Notes     string    `json:"notes,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Slot represents an available specialist appointment time window.
type Slot struct {
	ID          string `json:"id"`
	MasterID    string `json:"master_id"`
	MasterName  string `json:"master_name"`
	Date        string `json:"date"`         // Format: YYYY-MM-DD (e.g. "2026-10-31")
	StartTime   string `json:"start_time"`   // Format: HH:MM (e.g. "18:00")
	EndTime     string `json:"end_time"`     // Format: HH:MM (e.g. "19:00")
	ServiceType string `json:"service_type"` // e.g. "выездное обслуживание"
	IsAvailable bool   `json:"is_available"`
	ReservedBy  string `json:"reserved_by,omitempty"` // OrderID if booked
}

// Order represents an appointment/order created by the agent or system.
type Order struct {
	ID             string      `json:"id"`
	SessionID      string      `json:"session_id"`
	ClientID       string      `json:"client_id"`
	ClientName     string      `json:"client_name,omitempty"`
	ClientPhone    string      `json:"client_phone,omitempty"`
	MasterID       string      `json:"master_id,omitempty"`
	SlotID         string      `json:"slot_id,omitempty"`
	ServiceType    string      `json:"service_type"`
	Address        string      `json:"address"`
	Date           string      `json:"date"`        // Target date (e.g. "2026-10-31")
	TimeWindow     string      `json:"time_window"` // Target window (e.g. "18:00-19:00" or "после 18:00")
	Status         OrderStatus `json:"status"`
	Notes          string      `json:"notes,omitempty"`
	IdempotencyKey string      `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// AuditAction defines the operation performed.
type AuditAction string

const (
	AuditActionCreateOrder AuditAction = "CREATE_ORDER"
	AuditActionUpdateOrder AuditAction = "UPDATE_ORDER"
	AuditActionCancelOrder AuditAction = "CANCEL_ORDER"
	AuditActionReserveSlot AuditAction = "RESERVE_SLOT"
	AuditActionReleaseSlot AuditAction = "RELEASE_SLOT"
	AuditActionResetState  AuditAction = "RESET_STATE"
)

// AuditEvent records exact state mutations and API outcomes.
// Payload and state fields are independent JSON snapshots when read from the store.
// Crucial for testbench verification ("отдельно ответ API и реальное изменение").
type AuditEvent struct {
	ID             string      `json:"id"`
	SessionID      string      `json:"session_id"`
	Timestamp      time.Time   `json:"timestamp"`
	Action         AuditAction `json:"action"`
	EntityType     string      `json:"entity_type"` // "order", "slot", "client", "session"
	EntityID       string      `json:"entity_id"`
	Method         string      `json:"method"`
	Path           string      `json:"path"`
	IdempotencyKey string      `json:"idempotency_key,omitempty"`
	RequestPayload any         `json:"request_payload,omitempty"`
	ResponseStatus int         `json:"response_status"`
	BeforeState    any         `json:"before_state,omitempty"`
	AfterState     any         `json:"after_state,omitempty"`
	FaultInjected  string      `json:"fault_injected,omitempty"`
	ErrorMessage   string      `json:"error_message,omitempty"`
}

// FaultType defines simulated infrastructure and business failures.
type FaultType string

const (
	// FaultTimeoutAfterSave saves the state mutation to DB, but returns 504 Gateway Timeout
	// to simulate upstream/network drop. This is the canonical test scenario from text.pdf!
	FaultTimeoutAfterSave FaultType = "timeout_after_save"

	// FaultTimeoutBeforeSave times out without saving changes.
	FaultTimeoutBeforeSave FaultType = "timeout_before_save"

	// FaultDelay introduces artificial latency before processing.
	FaultDelay FaultType = "delay"

	// FaultConflict returns 409 Conflict.
	FaultConflict FaultType = "conflict"

	// FaultServerError returns 500 Internal Server Error.
	FaultServerError FaultType = "server_error"

	// FaultRateLimit returns 429 Too Many Requests.
	FaultRateLimit FaultType = "rate_limit"
)

// FaultRule defines a controlled failure injected into API interactions.
type FaultRule struct {
	ID           string    `json:"id"`
	FaultType    FaultType `json:"fault_type"`
	Method       string    `json:"method"`           // HTTP method filter (e.g. "POST" or "*")
	PathPattern  string    `json:"path_pattern"`     // Path pattern filter (e.g. "/api/v1/orders" or "*")
	DelayMs      int       `json:"delay_ms"`         // Delay before responding (ms)
	HttpStatus   int       `json:"http_status"`      // Status code to return (e.g. 504)
	ErrorMessage string    `json:"error_message"`    // Error message
	MaxTriggers  int       `json:"max_triggers"`     // Max executions (e.g. 1 means fail on 1st call, allow retry)
	TriggerCount int       `json:"trigger_count"`    // Times this fault has triggered
	Active       *bool     `json:"active,omitempty"` // nil enables the rule; false explicitly disables it
}

// StateSnapshot captures the full state of a test session at a point in time.
type StateSnapshot struct {
	SessionID         string            `json:"session_id"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	Clients           map[string]Client `json:"clients"`
	Slots             map[string]Slot   `json:"slots"`
	Orders            map[string]Order  `json:"orders"`
	ActiveOrdersCount int               `json:"active_orders_count"`
	TotalOrdersCount  int               `json:"total_orders_count"`
}

// VerificationCriteria provides assertions to evaluate test results.
type VerificationCriteria struct {
	ExpectedActiveOrdersCount *int   `json:"expected_active_orders_count,omitempty"`
	ClientID                  string `json:"client_id,omitempty"`
	DisallowDuplicates        bool   `json:"disallow_duplicates"`
	ExpectedAddress           string `json:"expected_address,omitempty"`
	ExpectedTimeWindow        string `json:"expected_time_window,omitempty"`
	ExpectedDate              string `json:"expected_date,omitempty"`
	ExpectedStatus            string `json:"expected_status,omitempty"`
}

// AssertionResult records individual assertion outcome with evidence.
type AssertionResult struct {
	Name    string   `json:"name"`
	Passed  bool     `json:"passed"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

// VerificationReport contains complete results of session verification.
type VerificationReport struct {
	SessionID    string            `json:"session_id"`
	OverallPass  bool              `json:"overall_pass"`
	CheckedAt    time.Time         `json:"checked_at"`
	Assertions   []AssertionResult `json:"assertions"`
	TotalOrders  int               `json:"total_orders"`
	ActiveOrders int               `json:"active_orders"`
}
