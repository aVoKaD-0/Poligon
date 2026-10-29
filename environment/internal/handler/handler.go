package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"poligon/environment/internal/domain"
	"poligon/environment/internal/service"
	"poligon/environment/internal/store"
)

// Handler handles HTTP requests for Environment CRM and Test Orchestration APIs.
type Handler struct {
	manager *store.Manager
	svc     *service.Service
}

// NewHandler creates a new HTTP handler.
func NewHandler(m *store.Manager, s *service.Service) *Handler {
	return &Handler{
		manager: m,
		svc:     s,
	}
}

// ExtractSessionID retrieves session ID from Header, Query, or default.
func (h *Handler) ExtractSessionID(r *http.Request) string {
	// 1. Check path if matched
	if p := r.PathValue("session_id"); p != "" {
		return p
	}
	// 2. Check HTTP Header
	if val := r.Header.Get("X-Session-ID"); val != "" {
		return strings.TrimSpace(val)
	}
	// 3. Check Query parameter
	if val := r.URL.Query().Get("session_id"); val != "" {
		return strings.TrimSpace(val)
	}
	return "default"
}

// WriteJSON sends a JSON response.
func (h *Handler) WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// WriteError sends a JSON error response.
func (h *Handler) WriteError(w http.ResponseWriter, status int, code, message string) {
	h.WriteJSON(w, status, map[string]string{
		"error":   code,
		"message": message,
	})
}

// Health checks service status.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	h.WriteJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "poligon-environment",
	})
}

// --- Session Orchestration Endpoints ---

// InitSessionPayload request payload for POST /api/v1/sessions
type InitSessionPayload struct {
	SessionID  string             `json:"session_id"`
	Clients    []domain.Client    `json:"clients,omitempty"`
	Slots      []domain.Slot      `json:"slots,omitempty"`
	FaultRules []domain.FaultRule `json:"fault_rules,omitempty"`
}

func (h *Handler) CreateOrInitSession(w http.ResponseWriter, r *http.Request) {
	var payload InitSessionPayload
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&payload)
	}
	sessionID := payload.SessionID
	if sessionID == "" {
		sessionID = h.ExtractSessionID(r)
	}

	sess := h.manager.InitSession(sessionID, payload.Clients, payload.Slots, payload.FaultRules)
	h.WriteJSON(w, http.StatusCreated, map[string]any{
		"session_id": sess.SessionID,
		"status":     "initialized",
		"snapshot":   sess.GetSnapshot(),
	})
}

func (h *Handler) ListSessions(w http.ResponseWriter, r *http.Request) {
	ids := h.manager.ListSessionIDs()
	h.WriteJSON(w, http.StatusOK, map[string]any{
		"sessions": ids,
		"count":    len(ids),
	})
}

func (h *Handler) GetSessionState(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	sess := h.manager.GetOrCreate(sessionID)
	h.WriteJSON(w, http.StatusOK, sess.GetSnapshot())
}

func (h *Handler) GetSessionAudit(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	sess := h.manager.GetOrCreate(sessionID)
	h.WriteJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID,
		"events":     sess.GetAuditLog(),
		"count":      len(sess.GetAuditLog()),
	})
}

func (h *Handler) ResetSession(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	sess := h.manager.GetOrCreate(sessionID)
	sess.Reset()
	h.WriteJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID,
		"status":     "reset_completed",
		"snapshot":   sess.GetSnapshot(),
	})
}

func (h *Handler) AddFaultRule(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	sess := h.manager.GetOrCreate(sessionID)

	var rule domain.FaultRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		h.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	sess.Faults().AddRule(rule)
	h.WriteJSON(w, http.StatusCreated, map[string]any{
		"status": "rule_added",
		"rules":  sess.Faults().GetRules(),
	})
}

func (h *Handler) ClearFaultRules(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	sess := h.manager.GetOrCreate(sessionID)
	sess.Faults().ClearRules()
	h.WriteJSON(w, http.StatusOK, map[string]any{
		"status": "rules_cleared",
	})
}

func (h *Handler) VerifySession(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	sess := h.manager.GetOrCreate(sessionID)

	var criteria domain.VerificationCriteria
	if err := json.NewDecoder(r.Body).Decode(&criteria); err != nil {
		h.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	report := sess.Verify(criteria)
	status := http.StatusOK
	if !report.OverallPass {
		status = http.StatusExpectationFailed
	}
	h.WriteJSON(w, status, report)
}

// --- CRM Business Endpoints ---

func (h *Handler) ListClients(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	clients := h.svc.ListClients(sessionID)
	h.WriteJSON(w, http.StatusOK, map[string]any{
		"clients": clients,
		"count":   len(clients),
	})
}

func (h *Handler) GetClient(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	clientID := r.PathValue("id")
	client, err := h.svc.GetClient(sessionID, clientID)
	if err != nil {
		h.WriteError(w, http.StatusNotFound, "client_not_found", "Client not found")
		return
	}
	h.WriteJSON(w, http.StatusOK, client)
}

func (h *Handler) ListSlots(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	date := r.URL.Query().Get("date")
	serviceType := r.URL.Query().Get("service")
	onlyAvailable := r.URL.Query().Get("available") != "false"

	slots := h.svc.ListSlots(sessionID, date, serviceType, onlyAvailable)
	h.WriteJSON(w, http.StatusOK, map[string]any{
		"slots": slots,
		"count": len(slots),
	})
}

func (h *Handler) ListOrders(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	clientID := r.URL.Query().Get("client_id")
	status := domain.OrderStatus(r.URL.Query().Get("status"))

	orders := h.svc.ListOrders(sessionID, clientID, status)
	h.WriteJSON(w, http.StatusOK, map[string]any{
		"orders": orders,
		"count":  len(orders),
	})
}

func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	orderID := r.PathValue("id")

	order, err := h.svc.GetOrder(sessionID, orderID)
	if err != nil {
		h.WriteError(w, http.StatusNotFound, "order_not_found", "Order not found")
		return
	}
	h.WriteJSON(w, http.StatusOK, order)
}

func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	sess := h.manager.GetOrCreate(sessionID)

	// Check manual header fault injection override
	if headerFault := r.Header.Get("X-Inject-Fault"); headerFault != "" {
		if headerFault == string(domain.FaultConflict) {
			h.WriteError(w, http.StatusConflict, "conflict", "Manual fault injected: conflict")
			return
		}
		if headerFault == string(domain.FaultServerError) {
			h.WriteError(w, http.StatusInternalServerError, "server_error", "Manual fault injected: internal server error")
			return
		}
	}

	// 1. Evaluate Pre-Request Faults
	postSaveRule, injectedErr := sess.Faults().MatchPreRequest(r.Method, r.URL.Path)
	if injectedErr != nil {
		sess.AppendAudit(domain.AuditEvent{
			Action:         domain.AuditActionCreateOrder,
			EntityType:     "order",
			Method:         r.Method,
			Path:           r.URL.Path,
			ResponseStatus: injectedErr.StatusCode,
			FaultInjected:  string(injectedErr.FaultType),
			ErrorMessage:   injectedErr.Message,
		})
		h.WriteError(w, injectedErr.StatusCode, string(injectedErr.FaultType), injectedErr.Message)
		return
	}

	// Also check if manual header requested timeout_after_save
	if r.Header.Get("X-Inject-Fault") == string(domain.FaultTimeoutAfterSave) && postSaveRule == nil {
		postSaveRule = &domain.FaultRule{
			FaultType:    domain.FaultTimeoutAfterSave,
			HttpStatus:   504,
			ErrorMessage: "Manual fault override: timeout_after_save",
		}
	}

	// 2. Decode Request Body
	var req service.CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	// Idempotency key can also come from header
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}

	// 3. Execute business logic in Service
	order, isIdempotent, err := h.svc.CreateOrder(sessionID, req)
	if err != nil {
		if errors.Is(err, service.ErrConflict) {
			h.WriteError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		if errors.Is(err, service.ErrNotFound) {
			h.WriteError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if errors.Is(err, service.ErrBadRequest) {
			h.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		h.WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// 4. Handle Post-Save Fault (e.g. Timeout After Save!)
	if postSaveRule != nil && !isIdempotent {
		postErr := sess.Faults().HandlePostSave(postSaveRule)
		if postErr != nil {
			// Record that order was SAVED, but caller received timeout!
			sess.AppendAudit(domain.AuditEvent{
				Action:         domain.AuditActionCreateOrder,
				EntityType:     "order",
				EntityID:       order.ID,
				Method:         r.Method,
				Path:           r.URL.Path,
				IdempotencyKey: req.IdempotencyKey,
				ResponseStatus: postErr.StatusCode,
				AfterState:     order,
				FaultInjected:  string(postErr.FaultType),
				ErrorMessage:   postErr.Message,
			})
			h.WriteError(w, postErr.StatusCode, string(postErr.FaultType), postErr.Message)
			return
		}
	}

	status := http.StatusCreated
	if isIdempotent {
		status = http.StatusOK
	}
	h.WriteJSON(w, status, order)
}

func (h *Handler) UpdateOrder(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	sess := h.manager.GetOrCreate(sessionID)
	orderID := r.PathValue("id")

	// Pre-request fault evaluation
	_, injectedErr := sess.Faults().MatchPreRequest(r.Method, r.URL.Path)
	if injectedErr != nil {
		sess.AppendAudit(domain.AuditEvent{
			Action:         domain.AuditActionUpdateOrder,
			EntityType:     "order",
			EntityID:       orderID,
			Method:         r.Method,
			Path:           r.URL.Path,
			ResponseStatus: injectedErr.StatusCode,
			FaultInjected:  string(injectedErr.FaultType),
			ErrorMessage:   injectedErr.Message,
		})
		h.WriteError(w, injectedErr.StatusCode, string(injectedErr.FaultType), injectedErr.Message)
		return
	}

	var req service.UpdateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	order, err := h.svc.UpdateOrder(sessionID, orderID, req)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.WriteError(w, http.StatusNotFound, "order_not_found", err.Error())
			return
		}
		if errors.Is(err, service.ErrConflict) {
			h.WriteError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		h.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	h.WriteJSON(w, http.StatusOK, order)
}

func (h *Handler) CancelOrder(w http.ResponseWriter, r *http.Request) {
	sessionID := h.ExtractSessionID(r)
	sess := h.manager.GetOrCreate(sessionID)
	orderID := r.PathValue("id")

	// Pre-request fault evaluation
	_, injectedErr := sess.Faults().MatchPreRequest(r.Method, r.URL.Path)
	if injectedErr != nil {
		sess.AppendAudit(domain.AuditEvent{
			Action:         domain.AuditActionCancelOrder,
			EntityType:     "order",
			EntityID:       orderID,
			Method:         r.Method,
			Path:           r.URL.Path,
			ResponseStatus: injectedErr.StatusCode,
			FaultInjected:  string(injectedErr.FaultType),
			ErrorMessage:   injectedErr.Message,
		})
		h.WriteError(w, injectedErr.StatusCode, string(injectedErr.FaultType), injectedErr.Message)
		return
	}

	order, err := h.svc.CancelOrder(sessionID, orderID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.WriteError(w, http.StatusNotFound, "order_not_found", err.Error())
			return
		}
		h.WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	h.WriteJSON(w, http.StatusOK, order)
}
