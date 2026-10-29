package handler

import (
	"net/http"
)

// NewRouter constructs and registers all HTTP endpoints.
func NewRouter(h *Handler) http.Handler {
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("GET /api/v1/health", h.Health)

	// Session orchestration endpoints
	mux.HandleFunc("POST /api/v1/sessions", h.CreateOrInitSession)
	mux.HandleFunc("GET /api/v1/sessions", h.ListSessions)
	mux.HandleFunc("GET /api/v1/sessions/{session_id}/state", h.GetSessionState)
	mux.HandleFunc("GET /api/v1/sessions/{session_id}/audit", h.GetSessionAudit)
	mux.HandleFunc("POST /api/v1/sessions/{session_id}/reset", h.ResetSession)
	mux.HandleFunc("POST /api/v1/sessions/{session_id}/faults", h.AddFaultRule)
	mux.HandleFunc("DELETE /api/v1/sessions/{session_id}/faults", h.ClearFaultRules)
	mux.HandleFunc("POST /api/v1/sessions/{session_id}/verify", h.VerifySession)

	// Global session state shortcuts (using X-Session-ID header)
	mux.HandleFunc("GET /api/v1/state", h.GetSessionState)
	mux.HandleFunc("GET /api/v1/audit", h.GetSessionAudit)
	mux.HandleFunc("POST /api/v1/reset", h.ResetSession)
	mux.HandleFunc("POST /api/v1/verify", h.VerifySession)

	// CRM Business API (Session identified by X-Session-ID header or default)
	mux.HandleFunc("GET /api/v1/clients", h.ListClients)
	mux.HandleFunc("GET /api/v1/clients/{id}", h.GetClient)

	mux.HandleFunc("GET /api/v1/slots", h.ListSlots)

	mux.HandleFunc("GET /api/v1/orders", h.ListOrders)
	mux.HandleFunc("GET /api/v1/orders/{id}", h.GetOrder)
	mux.HandleFunc("POST /api/v1/orders", h.CreateOrder)
	mux.HandleFunc("PATCH /api/v1/orders/{id}", h.UpdateOrder)
	mux.HandleFunc("POST /api/v1/orders/{id}/cancel", h.CancelOrder)
	mux.HandleFunc("DELETE /api/v1/orders/{id}", h.CancelOrder)

	// Additional session-prefixed routes for explicit routing without headers
	mux.HandleFunc("GET /api/v1/sessions/{session_id}/clients", h.ListClients)
	mux.HandleFunc("GET /api/v1/sessions/{session_id}/clients/{id}", h.GetClient)
	mux.HandleFunc("GET /api/v1/sessions/{session_id}/slots", h.ListSlots)
	mux.HandleFunc("GET /api/v1/sessions/{session_id}/orders", h.ListOrders)
	mux.HandleFunc("GET /api/v1/sessions/{session_id}/orders/{id}", h.GetOrder)
	mux.HandleFunc("POST /api/v1/sessions/{session_id}/orders", h.CreateOrder)
	mux.HandleFunc("PATCH /api/v1/sessions/{session_id}/orders/{id}", h.UpdateOrder)
	mux.HandleFunc("POST /api/v1/sessions/{session_id}/orders/{id}/cancel", h.CancelOrder)
	mux.HandleFunc("DELETE /api/v1/sessions/{session_id}/orders/{id}", h.CancelOrder)

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
