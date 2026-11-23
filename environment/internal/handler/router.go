package handler

import (
	"net/http"
)

// NewRouter constructs and registers all HTTP endpoints.
func NewRouter(h *Handler) http.Handler {
	return NewGuardedRouter(h, func(next http.HandlerFunc) http.HandlerFunc { return next })
}

// NewGuardedRouter applies the guard after path variables have been resolved.
func NewGuardedRouter(h *Handler, guard func(http.HandlerFunc) http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern string, next http.HandlerFunc) { mux.HandleFunc(pattern, guard(next)) }

	// Health check
	handle("GET /health", h.Health)
	handle("GET /api/v1/health", h.Health)

	// Session orchestration endpoints
	handle("POST /api/v1/sessions", h.CreateOrInitSession)
	handle("GET /api/v1/sessions", h.ListSessions)
	handle("GET /api/v1/sessions/{session_id}/state", h.GetSessionState)
	handle("GET /api/v1/sessions/{session_id}/audit", h.GetSessionAudit)
	handle("POST /api/v1/sessions/{session_id}/reset", h.ResetSession)
	handle("POST /api/v1/sessions/{session_id}/faults", h.AddFaultRule)
	handle("DELETE /api/v1/sessions/{session_id}/faults", h.ClearFaultRules)
	handle("POST /api/v1/sessions/{session_id}/verify", h.VerifySession)

	// Global session state shortcuts (using X-Session-ID header)
	handle("GET /api/v1/state", h.GetSessionState)
	handle("GET /api/v1/audit", h.GetSessionAudit)
	handle("POST /api/v1/reset", h.ResetSession)
	handle("POST /api/v1/verify", h.VerifySession)

	// CRM Business API (Session identified by X-Session-ID header or default)
	handle("GET /api/v1/clients", h.ListClients)
	handle("GET /api/v1/clients/{id}", h.GetClient)

	handle("GET /api/v1/slots", h.ListSlots)

	handle("GET /api/v1/orders", h.ListOrders)
	handle("GET /api/v1/orders/{id}", h.GetOrder)
	handle("POST /api/v1/orders", h.CreateOrder)
	handle("PATCH /api/v1/orders/{id}", h.UpdateOrder)
	handle("POST /api/v1/orders/{id}/cancel", h.CancelOrder)
	handle("DELETE /api/v1/orders/{id}", h.CancelOrder)

	// Additional session-prefixed routes for explicit routing without headers
	handle("GET /api/v1/sessions/{session_id}/clients", h.ListClients)
	handle("GET /api/v1/sessions/{session_id}/clients/{id}", h.GetClient)
	handle("GET /api/v1/sessions/{session_id}/slots", h.ListSlots)
	handle("GET /api/v1/sessions/{session_id}/orders", h.ListOrders)
	handle("GET /api/v1/sessions/{session_id}/orders/{id}", h.GetOrder)
	handle("POST /api/v1/sessions/{session_id}/orders", h.CreateOrder)
	handle("PATCH /api/v1/sessions/{session_id}/orders/{id}", h.UpdateOrder)
	handle("POST /api/v1/sessions/{session_id}/orders/{id}/cancel", h.CancelOrder)
	handle("DELETE /api/v1/sessions/{session_id}/orders/{id}", h.CancelOrder)

	// Logging middleware
	return loggingMiddleware(mux)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers so frontend or runner can connect smoothly
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-ID, Idempotency-Key, X-Inject-Fault")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
