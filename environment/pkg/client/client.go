package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"poligon/environment/internal/domain"
	"poligon/environment/internal/service"
)

// Client is a Go client for interacting with the Environment service.
type Client struct {
	baseURL    string
	httpClient *http.Client
	sessionID  string
}

// NewClient creates a new Environment API client.
func NewClient(baseURL, sessionID string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		sessionID: sessionID,
	}
}

// SetSessionID updates the active session ID for requests.
func (c *Client) SetSessionID(sessionID string) {
	c.sessionID = sessionID
}

func (c *Client) do(ctx context.Context, method, path string, body any, idempotencyKey string) (*http.Response, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.sessionID != "" {
		req.Header.Set("X-Session-ID", c.sessionID)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("read response: %w", err)
	}

	return resp, respBytes, nil
}

// InitSession initializes or resets a session with fixtures and fault rules.
func (c *Client) InitSession(ctx context.Context, sessionID string, clients []domain.Client, slots []domain.Slot, faultRules []domain.FaultRule) (*domain.StateSnapshot, error) {
	payload := map[string]any{
		"session_id":  sessionID,
		"clients":     clients,
		"slots":       slots,
		"fault_rules": faultRules,
	}

	resp, body, err := c.do(ctx, http.MethodPost, "/api/v1/sessions", payload, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("init session failed with status %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		Snapshot domain.StateSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	c.sessionID = sessionID
	return &res.Snapshot, nil
}

// AddFaultRule registers a controlled failure for the current session.
func (c *Client) AddFaultRule(ctx context.Context, rule domain.FaultRule) error {
	resp, body, err := c.do(ctx, http.MethodPost, "/api/v1/sessions/"+c.sessionID+"/faults", rule, "")
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("add fault rule failed: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// GetState fetches current session state snapshot.
func (c *Client) GetState(ctx context.Context) (*domain.StateSnapshot, error) {
	resp, body, err := c.do(ctx, http.MethodGet, "/api/v1/sessions/"+c.sessionID+"/state", nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get state failed: status %d: %s", resp.StatusCode, string(body))
	}
	var snap domain.StateSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// GetAuditLog retrieves session audit events.
func (c *Client) GetAuditLog(ctx context.Context) ([]domain.AuditEvent, error) {
	resp, body, err := c.do(ctx, http.MethodGet, "/api/v1/sessions/"+c.sessionID+"/audit", nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get audit failed: status %d: %s", resp.StatusCode, string(body))
	}
	var res struct {
		Events []domain.AuditEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	return res.Events, nil
}

// ResetSession resets session state back to initial state.
func (c *Client) ResetSession(ctx context.Context) error {
	resp, body, err := c.do(ctx, http.MethodPost, "/api/v1/sessions/"+c.sessionID+"/reset", nil, "")
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("reset session failed: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// Verify evaluates criteria against current session state.
func (c *Client) Verify(ctx context.Context, criteria domain.VerificationCriteria) (*domain.VerificationReport, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/api/v1/sessions/"+c.sessionID+"/verify", criteria, "")
	if err != nil {
		return nil, err
	}
	var report domain.VerificationReport
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// SearchSlots lists available slots.
func (c *Client) SearchSlots(ctx context.Context, date, serviceType string) ([]domain.Slot, error) {
	params := url.Values{}
	if date != "" {
		params.Set("date", date)
	}
	if serviceType != "" {
		params.Set("service", serviceType)
	}
	path := "/api/v1/slots"
	if q := params.Encode(); q != "" {
		path += "?" + q
	}
	resp, body, err := c.do(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search slots failed: status %d: %s", resp.StatusCode, string(body))
	}
	var res struct {
		Slots []domain.Slot `json:"slots"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	return res.Slots, nil
}

// CreateOrder books an appointment.
func (c *Client) CreateOrder(ctx context.Context, req service.CreateOrderRequest, idempotencyKey string) (*domain.Order, int, error) {
	resp, body, err := c.do(ctx, http.MethodPost, "/api/v1/orders", req, idempotencyKey)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("create order returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	var ord domain.Order
	if err := json.Unmarshal(body, &ord); err != nil {
		return nil, resp.StatusCode, err
	}
	return &ord, resp.StatusCode, nil
}

// UpdateOrder reschedules or modifies order.
func (c *Client) UpdateOrder(ctx context.Context, orderID string, req service.UpdateOrderRequest) (*domain.Order, error) {
	resp, body, err := c.do(ctx, http.MethodPatch, "/api/v1/orders/"+orderID, req, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update order returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	var ord domain.Order
	if err := json.Unmarshal(body, &ord); err != nil {
		return nil, err
	}
	return &ord, nil
}

// CancelOrder cancels an appointment.
func (c *Client) CancelOrder(ctx context.Context, orderID string) (*domain.Order, error) {
	resp, body, err := c.do(ctx, http.MethodPost, "/api/v1/orders/"+orderID+"/cancel", nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cancel order returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	var ord domain.Order
	if err := json.Unmarshal(body, &ord); err != nil {
		return nil, err
	}
	return &ord, nil
}
