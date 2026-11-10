package transport

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/poligon/runner/internal/execution"
)

type Environment struct {
	Client  *http.Client
	BaseURL string
	Timeout time.Duration
	Limit   int64
}

type crm struct {
	*Environment
	id string
}

func (e *Environment) Open(ctx context.Context, s execution.Scope, setup execution.Setup) (execution.Workspace, error) {
	if !s.Valid() {
		return nil, execution.ErrDependency
	}
	var definition struct {
		Version         int    `json:"schema_version"`
		Template        string `json:"template_id"`
		TemplateVersion string `json:"template_version"`
		Capability      string `json:"capability_version"`
	}
	if execution.DecodeStrict(setup.Definition, &definition) != nil || definition.Version != 1 ||
		definition.Template != "field-service" || definition.TemplateVersion != "v1" ||
		definition.Capability != "field-service.v1" ||
		setup.Template != definition.Template || setup.TemplateVersion != definition.TemplateVersion {
		return nil, execution.ErrUnsupported
	}
	var fixture struct {
		Clients json.RawMessage `json:"clients,omitempty"`
		Slots   json.RawMessage `json:"slots,omitempty"`
	}
	if execution.DecodeStrict(setup.Fixture, &fixture) != nil {
		return nil, execution.ErrUnsupported
	}
	id := fmt.Sprintf("%s-%s-%s-%d", s.ProjectID, s.RunID, s.SessionID, s.Attempt)
	body, err := json.Marshal(struct {
		SessionID string          `json:"session_id"`
		Clients   json.RawMessage `json:"clients,omitempty"`
		Slots     json.RawMessage `json:"slots,omitempty"`
		Faults    json.RawMessage `json:"fault_rules"`
	}{id, fixture.Clients, fixture.Slots, setup.Faults})
	if err != nil {
		return nil, execution.ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	response, err := sendRequest(ctx, e.Client, http.MethodPost, e.BaseURL+"/api/v1/sessions", body, make(http.Header))
	if err != nil {
		return nil, execution.ErrDependency
	}
	defer response.Body.Close()
	// The pinned service echoes the fixture as maps. Each input entity with a
	// nonempty ID takes at least 10 bytes and gains fewer than 150 fixed bytes
	// plus the repeated ID. The base allowance covers the built-in fixtures.
	limit := (1 << 20) + 18*int64(len(body))
	if response.StatusCode != http.StatusCreated || !initialized(response.Body, limit, id) {
		return nil, execution.ErrDependency
	}
	return &crm{Environment: e, id: id}, nil
}

func initialized(r io.Reader, limit int64, expectedID string) bool {
	bounded := &io.LimitedReader{R: r, N: limit + 1}
	dec := jsontext.NewDecoder(bounded)
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return false
	}
	var id, status string
	for dec.PeekKind() == '"' {
		key, err := dec.ReadToken()
		if err != nil {
			return false
		}
		switch key.String() {
		case "session_id":
			if tok, err := dec.ReadToken(); err == nil && tok.Kind() == '"' {
				id = tok.String()
			} else {
				return false
			}
		case "status":
			if tok, err := dec.ReadToken(); err == nil && tok.Kind() == '"' {
				status = tok.String()
			} else {
				return false
			}
		default:
			// Consume the snapshot without keeping a second copy in memory.
			for {
				if _, err := dec.ReadToken(); err != nil {
					return false
				}
				if dec.StackDepth() == 1 {
					break
				}
			}
		}
	}
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '}' {
		return false
	}
	_, err := dec.ReadToken()
	return err == io.EOF && bounded.N > 0 && id == expectedID && status == "initialized"
}

func (c *crm) Finalize(context.Context) (execution.Ref, execution.Ref, error) {
	// This Environment revision exposes raw state and audit, but does not publish Platform-owned refs.
	return execution.Ref{}, execution.Ref{}, execution.ErrUnsupported
}

var resourceID = regexp.MustCompile("^[A-Za-z0-9_-][A-Za-z0-9_.:-]{0,127}$")

func (c *crm) Call(ctx context.Context, call execution.Call) execution.Result {
	result := execution.Result{ID: call.ID}
	method, path, query, body, code := route(call)
	if code != "" {
		result.Error = code
		return result
	}
	if !headerValue(call.IdempotencyKey) || strings.TrimSpace(call.IdempotencyKey) != call.IdempotencyKey {
		result.Error = "invalid_arguments"
		return result
	}
	h := make(http.Header)
	h.Set("X-Session-ID", c.id)
	if method != http.MethodGet && call.IdempotencyKey != "" {
		h.Set("Idempotency-Key", call.IdempotencyKey)
	}
	target := c.BaseURL + "/api/v1/" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	status, raw, err := send(ctx, c.Client, method, target, body, h, c.Limit)
	result.Status = status
	if err != nil {
		result.Error = errCode(ctx, err)
	} else if !objectJSON(raw) {
		result.Error = "invalid_environment_response"
	} else {
		result.Body = raw
	}
	return result
}

type createArgs struct {
	ClientID string `json:"client_id"`
	Service  string `json:"service_type"`
	Address  string `json:"address"`
	Date     string `json:"date"`
	Window   string `json:"time_window"`
	Slot     string `json:"slot_id"`
	Notes    string `json:"notes"`
}

type updateArgs struct {
	ID      string  `json:"id"`
	Address *string `json:"address,omitempty"`
	Date    *string `json:"date,omitempty"`
	Window  *string `json:"time_window,omitempty"`
	Slot    *string `json:"slot_id,omitempty"`
	Notes   *string `json:"notes,omitempty"`
}

func route(call execution.Call) (method, path string, q url.Values, body []byte, code string) {
	invalid := func() (string, string, url.Values, []byte, string) { return "", "", nil, nil, "invalid_arguments" }
	switch call.Name {
	case "list_clients":
		if execution.DecodeStrict(call.Arguments, &struct{}{}) != nil {
			return invalid()
		}
		return http.MethodGet, "clients", nil, nil, ""
	case "get_client", "get_order", "cancel_order":
		var a struct {
			ID string `json:"id"`
		}
		if execution.DecodeStrict(call.Arguments, &a) != nil || !resourceID.MatchString(a.ID) {
			return invalid()
		}
		if call.Name == "get_client" {
			return http.MethodGet, "clients/" + a.ID, nil, nil, ""
		}
		if call.Name == "get_order" {
			return http.MethodGet, "orders/" + a.ID, nil, nil, ""
		}
		return http.MethodPost, "orders/" + a.ID + "/cancel", nil, []byte("{}"), ""
	case "search_slots":
		var a struct {
			Date      string `json:"date"`
			Service   string `json:"service"`
			Available *bool  `json:"available"`
		}
		if execution.DecodeStrict(call.Arguments, &a) != nil || !validDate(a.Date) || len(a.Service) > 256 {
			return invalid()
		}
		q = url.Values{"date": {a.Date}, "service": {a.Service}}
		if a.Available != nil && !*a.Available {
			q.Set("available", "false")
		}
		return http.MethodGet, "slots", q, nil, ""
	case "list_orders":
		var a struct {
			Client string `json:"client_id"`
			Status string `json:"status"`
		}
		if execution.DecodeStrict(call.Arguments, &a) != nil || (a.Client != "" && !resourceID.MatchString(a.Client)) ||
			len(a.Status) > 64 {
			return invalid()
		}
		return http.MethodGet, "orders", url.Values{"client_id": {a.Client}, "status": {a.Status}}, nil, ""
	case "create_order":
		var a createArgs
		if execution.DecodeStrict(call.Arguments, &a) != nil || !resourceID.MatchString(a.ClientID) || strings.TrimSpace(a.Address) == "" ||
			!validDate(a.Date) || (a.Slot != "" && !resourceID.MatchString(a.Slot)) ||
			(a.Slot == "" && (a.Date == "" || strings.TrimSpace(a.Window) == "")) {
			return invalid()
		}
		return http.MethodPost, "orders", nil, call.Arguments, ""
	case "update_order":
		var a updateArgs
		if execution.DecodeStrict(call.Arguments, &a) != nil || !resourceID.MatchString(a.ID) ||
			(a.Address == nil && a.Date == nil && a.Window == nil && a.Slot == nil && a.Notes == nil) ||
			(a.Date != nil && !validDate(*a.Date)) || (a.Slot != nil && *a.Slot != "" && !resourceID.MatchString(*a.Slot)) {
			return invalid()
		}
		path = "orders/" + a.ID
		payload := struct {
			Address *string `json:"address,omitempty"`
			Date    *string `json:"date,omitempty"`
			Window  *string `json:"time_window,omitempty"`
			Slot    *string `json:"slot_id,omitempty"`
			Notes   *string `json:"notes,omitempty"`
		}{a.Address, a.Date, a.Window, a.Slot, a.Notes}
		body, _ = json.Marshal(payload)
		return http.MethodPatch, path, nil, body, ""
	default:
		return "", "", nil, nil, "unknown_tool"
	}
}

func validDate(date string) bool {
	if date == "" {
		return true
	}
	_, err := time.Parse("2006-01-02", date)
	return err == nil
}
