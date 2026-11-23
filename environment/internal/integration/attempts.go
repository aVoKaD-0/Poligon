package integration

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"

	pb "poligon/environment/gen/controlv1"
	env "poligon/environment/gen/environmentv1"
	"poligon/environment/internal/faults"
	"poligon/environment/internal/handler"
	"poligon/environment/internal/store"
	"poligon/environment/pkg/api"
)

var uuid = regexp.MustCompile("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$")
var crmID = regexp.MustCompile("^[A-Za-z0-9_-][A-Za-z0-9_.:-]{0,127}$")

type Scope struct {
	Project string `json:"project_id"`
	Run     string `json:"run_id"`
	Session string `json:"session_id"`
	Attempt uint32 `json:"attempt_no"`
}

func (s Scope) valid() bool {
	return uuid.MatchString(s.Project) && uuid.MatchString(s.Run) && uuid.MatchString(s.Session) && s.Attempt > 0
}

func (s Scope) proto() *pb.ExecutionContext {
	return &pb.ExecutionContext{ProjectId: s.Project, RunId: s.Run, SessionId: s.Session, AttemptNo: s.Attempt}
}

func scopeOf(c *pb.ExecutionContext) Scope {
	return Scope{c.GetProjectId(), c.GetRunId(), c.GetSessionId(), c.GetAttemptNo()}
}

type initial struct {
	Clients []api.Client `json:"clients"`
	Slots   []api.Slot   `json:"slots"`
}

func (f initial) validate() error {
	seen := make(map[string]bool)
	for _, c := range f.Clients {
		if c.ID == "" || seen[c.ID] {
			return fmt.Errorf("clients require unique nonempty ids")
		}
		seen[c.ID] = true
	}
	clear(seen)
	for _, s := range f.Slots {
		if s.ID == "" || seen[s.ID] {
			return fmt.Errorf("slots require unique nonempty ids")
		}
		seen[s.ID] = true
	}
	return nil
}

type opening struct {
	Context   Scope  `json:"context"`
	SessionID string `json:"session_id"`
	initial
	Rules []api.FaultRule `json:"fault_rules"`
}

func validateRules(rules []api.FaultRule) error {
	for _, rule := range rules {
		if err := faults.ValidateRule(rule); err != nil {
			return err
		}
	}
	return nil
}

func strictJSON(r io.Reader, target any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

func authorized(header, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(header), []byte("Bearer "+token)) == 1
}

type attempt struct {
	scope       Scope
	state       *store.SessionState
	fingerprint [32]byte
	mu          sync.Mutex
	closing     bool
	active      int
	drained     chan struct{}
	// Only the owner of publishing may freeze bytes and advance upload progress.
	publishing chan struct{}
	snapshot   *artifact
	audit      *artifact
}

func (a *attempt) enter() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return false
	}
	a.active++
	return true
}

func (a *attempt) leave() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active--
	if a.active == 0 && a.closing {
		close(a.drained)
	}
}

func (a *attempt) seal() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closing {
		a.closing = true
		if a.active == 0 {
			close(a.drained)
		}
	}
	return a.drained
}

type Server struct {
	env.UnimplementedEnvironmentServiceServer
	manager          *store.Manager
	runner, platform string
	uploader         uploader
	mu               sync.RWMutex
	byScope          map[Scope]*attempt
	byID             map[string]*attempt
}

func New(manager *store.Manager, runner, platform string, control pb.ControlServiceClient, controlToken string) *Server {
	return &Server{
		manager:  manager,
		runner:   runner,
		platform: platform,
		uploader: newUploader(control, controlToken),
		byScope:  make(map[Scope]*attempt),
		byID:     make(map[string]*attempt),
	}
}

func (s *Server) lookup(scope Scope, id string) *attempt {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a := s.byID[id]
	if a == nil || a.scope != scope {
		return nil
	}
	return a
}

func (s *Server) open(req opening) (*attempt, int) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, http.StatusBadRequest
	}
	sum := sha256.Sum256(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.byScope[req.Context]; a != nil {
		if a.fingerprint != sum {
			return nil, http.StatusConflict
		}
		return a, http.StatusOK
	}
	if s.byID[req.SessionID] != nil {
		return nil, http.StatusConflict
	}
	a := &attempt{
		scope:       req.Context,
		state:       s.manager.InitSession(req.SessionID, req.Clients, req.Slots, req.Rules),
		fingerprint: sum,
		drained:     make(chan struct{}),
		publishing:  make(chan struct{}, 1),
	}
	s.byID[req.SessionID] = a
	s.byScope[req.Context] = a
	return a, http.StatusCreated
}

func (s *Server) HTTP(h *handler.Handler) http.Handler {
	guarded := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !authorized(r.Header.Get("Authorization"), s.runner) {
				h.WriteError(w, 401, "unauthenticated", "Runner token required")
				return
			}
			if r.Pattern == "POST /api/v1/sessions" || r.Pattern == "GET /api/v1/sessions" {
				h.WriteError(w, 409, "attempt_required", "Use integration opening")
				return
			}
			var scope Scope
			id := r.Header.Get("X-Session-ID")
			if strictJSON(bytes.NewBufferString(r.Header.Get("X-Execution-Context")), &scope) != nil || !scope.valid() || id != h.ExtractSessionID(r) || (r.URL.Query().Has("session_id") && r.URL.Query().Get("session_id") != id) {
				h.WriteError(w, 403, "scope_mismatch", "Execution context and CRM id required")
				return
			}
			a := s.lookup(scope, id)
			if a == nil {
				h.WriteError(w, 403, "scope_mismatch", "Unknown attempt binding")
				return
			}
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				next(w, r)
				return
			}
			if !a.enter() {
				h.WriteError(w, 409, "attempt_closed", "Attempt is finalizing or finalized")
				return
			}
			defer a.leave()
			next(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("GET /api/v1/health", h.Health)
	mux.HandleFunc("POST /api/v1/attempts", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r.Header.Get("Authorization"), s.runner) {
			h.WriteError(w, 401, "unauthenticated", "Runner token required")
			return
		}
		var req opening
		if err := strictJSON(http.MaxBytesReader(w, r.Body, 4<<20), &req); err != nil {
			h.WriteError(w, 400, "invalid_opening", err.Error())
			return
		}
		if !req.Context.valid() || !crmID.MatchString(req.SessionID) {
			h.WriteError(w, 400, "invalid_scope", "Invalid execution context or CRM id")
			return
		}
		if err := req.initial.validate(); err != nil {
			h.WriteError(w, 400, "invalid_fixture", err.Error())
			return
		}
		if err := validateRules(req.Rules); err != nil {
			h.WriteError(w, 400, "invalid_fault_rule", err.Error())
			return
		}
		a, status := s.open(req)
		if a == nil {
			h.WriteError(w, status, "opening_conflict", "Scope or CRM id already bound with different inputs")
			return
		}
		h.WriteJSON(w, status, map[string]any{"context": req.Context, "session_id": req.SessionID, "status": "initialized", "snapshot": a.state.GetSnapshot()})
	})
	mux.Handle("/", handler.NewGuardedRouter(h, guarded))
	return mux
}
