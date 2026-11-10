package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/poligon/runner/contracts"
	pb "github.com/poligon/runner/contracts/controlv1"
)

const CommandSubject = "poligon.cmd.session.execute.v1"
const MaxEvent = 64 << 10

var (
	uuidPattern        = regexp.MustCompile("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$")
	shaPattern         = regexp.MustCompile("^[0-9a-f]{64}$")
	ErrProtocol        = errors.New("agent protocol error")
	ErrDependency      = errors.New("dependency unavailable")
	ErrUnsupported     = errors.New("integration contract unavailable")
	ErrLimit           = errors.New("evidence size limit exceeded")
	ErrMessageLimit    = errors.New("agent message size limit exceeded")
	errLease           = errors.New("execution lease lost")
	errCancelled       = errors.New("platform cancellation")
	errSessionDeadline = errors.New("session deadline reached")
)

func IDValid(v string) bool   { return uuidPattern.MatchString(v) }
func HashValid(v string) bool { return shaPattern.MatchString(v) }

// MarshalJSON preserves the wire field semantics without HTML-only escaping.
func MarshalJSON(v any) ([]byte, error) {
	return jsonv2.Marshal(v, json.DefaultOptionsV1(), jsontext.EscapeForHTML(false), jsontext.EscapeForJS(false))
}

type Scope struct {
	ProjectID string `json:"project_id"`
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	Attempt   uint32 `json:"attempt_no"`
}

func (s Scope) Valid() bool {
	return IDValid(s.ProjectID) && IDValid(s.RunID) && IDValid(s.SessionID) && s.Attempt > 0
}

func (s Scope) Proto() *pb.ExecutionContext {
	return &pb.ExecutionContext{ProjectId: s.ProjectID, RunId: s.RunID, SessionId: s.SessionID, AttemptNo: s.Attempt}
}

type Job struct {
	Scope
	Schema          int       `json:"schema_version"`
	MessageID       string    `json:"message_id"`
	ProducerEventID string    `json:"producer_event_id"`
	Type            string    `json:"type"`
	Producer        string    `json:"producer"`
	OccurredAt      time.Time `json:"occurred_at"`
	Traceparent     string    `json:"traceparent"`
	Payload         struct {
		Dispatch string `json:"dispatch_id"`
		PlanRef  string `json:"runtime_plan_ref"`
		PlanHash string `json:"runtime_plan_sha256"`
	} `json:"payload"`
}

func DecodeJob(subject string, raw []byte) (Job, error) {
	var j Job
	if subject != CommandSubject || len(raw) > MaxEvent || !utf8.Valid(raw) {
		return j, errors.New("invalid command subject or size")
	}
	// Match exact field names, reject duplicate keys and allow envelope extensions.
	if err := jsonv2.Unmarshal(raw, &j); err != nil || !j.Scope.Valid() ||
		j.Schema != 1 || j.Type != "session.execute.v1" || j.Producer != "platform" ||
		j.OccurredAt.IsZero() || !IDValid(j.MessageID) || !IDValid(j.ProducerEventID) ||
		!IDValid(j.Payload.Dispatch) || !IDValid(j.Payload.PlanRef) || !HashValid(j.Payload.PlanHash) {
		return Job{}, errors.New("invalid command envelope")
	}
	return j, nil
}

type Limits struct {
	Turns    int    `json:"max_client_turns"`
	Tools    int    `json:"max_tool_calls"`
	Duration int64  `json:"session_duration_ms"`
	Budget   *int64 `json:"budget_minor"`
}

type Versions struct {
	Agent       string `json:"agent_version_id"`
	Scenario    string `json:"scenario_version_id"`
	Environment string `json:"environment_version_id"`
	Simulator   string `json:"simulator_version_id"`
	Evaluator   string `json:"evaluator_version_id"`
}

type AgentConfig struct {
	Adapter       string          `json:"adapter"`
	Endpoint      string          `json:"endpoint"`
	CredentialRef string          `json:"credential_ref"`
	TimeoutMS     int             `json:"request_timeout_ms"`
	Config        json.RawMessage `json:"config"`
}

type Plan struct {
	Scope         Scope           `json:"context"`
	Schema        int             `json:"schema_version"`
	Versions      Versions        `json:"versions"`
	Seed          *int64          `json:"seed"`
	AgentJSON     json.RawMessage `json:"agent"`
	ScenarioJSON  json.RawMessage `json:"scenario"`
	Simulator     json.RawMessage `json:"simulator"`
	Environment   json.RawMessage `json:"environment"`
	Adapter       string          `json:"agent_adapter"`
	CredentialRef string          `json:"credential_revision_ref"`
	Limits        Limits          `json:"limits"`
	Agent         AgentConfig     `json:"-"`
	Scenario      struct {
		Modality        string          `json:"modality"`
		Environment     string          `json:"environment_version_id"`
		Simulator       string          `json:"simulator_version_id"`
		Template        string          `json:"template_id"`
		TemplateVersion string          `json:"template_version"`
		Fixture         json.RawMessage `json:"initial_fixture"`
		Faults          json.RawMessage `json:"fault_rules"`
		Actor           string          `json:"actor_customer_ref"`
		Clock           string          `json:"virtual_clock"`
		Zone            string          `json:"business_timezone"`
		Goal            string          `json:"client_goal"`
		Facts           json.RawMessage `json:"client_known_facts"`
		VoiceActions    json.RawMessage `json:"voice_actions"`
		Limits          Limits          `json:"limits"`
	} `json:"-"`
}

func ParsePlan(job Job, response *pb.LeaseResponse, schemas contracts.Schemas, limit int) (Plan, error) {
	var p Plan
	raw := response.GetRuntimePlanJson()
	if !response.GetAccepted() || response.GetRuntimePlanRef() != job.Payload.PlanRef ||
		response.GetRuntimePlanSha256() != job.Payload.PlanHash || len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) {
		return p, errors.New("runtime plan identity or size mismatch")
	}
	canonical, err := canonicalPlan(raw)
	if err != nil {
		return p, errors.New("invalid canonical runtime plan")
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != job.Payload.PlanHash {
		return p, errors.New("runtime plan checksum mismatch")
	}
	if jsonv2.Unmarshal(canonical, &p) != nil || p.Scope != job.Scope || p.Schema != 1 ||
		p.Seed == nil || *p.Seed < -9007199254740991 || *p.Seed > 9007199254740991 ||
		!IDValid(p.Versions.Agent) || !IDValid(p.Versions.Scenario) || !IDValid(p.Versions.Environment) ||
		!IDValid(p.Versions.Simulator) || !IDValid(p.Versions.Evaluator) ||
		!jsonObject(p.Environment) || !jsonObject(p.Simulator) {
		return Plan{}, errors.New("invalid frozen execution inputs")
	}
	if contracts.Validate(schemas.Agent, p.AgentJSON) != nil ||
		contracts.Validate(schemas.Scenario, p.ScenarioJSON) != nil ||
		jsonv2.Unmarshal(p.AgentJSON, &p.Agent) != nil || jsonv2.Unmarshal(p.ScenarioJSON, &p.Scenario) != nil {
		return Plan{}, errors.New("invalid agent or scenario definition")
	}
	l, expected := p.Limits, p.Scenario.Limits
	if l.Turns != expected.Turns || l.Tools != expected.Tools || l.Duration != expected.Duration ||
		(l.Budget == nil) != (expected.Budget == nil) || (l.Budget != nil && *l.Budget != *expected.Budget) ||
		p.Versions.Environment != p.Scenario.Environment || p.Versions.Simulator != p.Scenario.Simulator ||
		p.Adapter != p.Agent.Adapter || p.CredentialRef != p.Agent.CredentialRef ||
		(p.Scenario.Modality == "text") != (p.Agent.Adapter == "http-json-v1") {
		return Plan{}, errors.New("inconsistent frozen execution inputs")
	}
	return p, nil
}

func canonicalPlan(raw []byte) ([]byte, error) {
	if err := validateJSON(raw, 0); err != nil {
		return nil, err
	}
	canonical := jsontext.Value(bytes.Clone(raw))
	if err := canonical.Canonicalize(); err != nil {
		return nil, err
	}
	return canonical, nil
}

func validateJSON(raw []byte, maxMembers int64) error {
	// Bound nesting and reject numbers outside the finite range used by JCS.
	// The journal consumer also needs a member bound for its JCS implementation.
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := dec.ReadToken()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		depth := dec.StackDepth()
		if depth > 64 {
			return errors.New("JSON nesting limit exceeded")
		}
		if kind, length := dec.StackIndex(depth); maxMembers > 0 && kind == '{' && length > 2*maxMembers {
			return errors.New("JSON object member limit exceeded")
		}
		switch token.Kind() {
		case '"':
			if strings.IndexByte(token.String(), 0) >= 0 {
				return errors.New("JSON string contains a NUL character")
			}
		case '0':
			if err := validateJSONNumber(token.String()); err != nil {
				return err
			}
		}
	}
}

func validateJSONNumber(number string) error {
	if _, err := strconv.ParseFloat(number, 64); err != nil {
		return err
	}
	// Platform persists journal records as PostgreSQL jsonb. Its numeric scale
	// can overflow even when a float64 parser rounds the value to zero.
	const maxScale = 16383
	var exponent int
	if pos := strings.IndexAny(number, "eE"); pos >= 0 {
		var err error
		exponent, err = strconv.Atoi(number[pos+1:])
		if err != nil || exponent < -maxScale || exponent > 131072 {
			return errors.New("JSON numeric exponent out of range")
		}
		number = number[:pos]
	}
	if pos := strings.IndexByte(number, '.'); pos >= 0 && len(number)-pos-1-exponent > maxScale {
		return errors.New("JSON numeric scale out of range")
	}
	return nil
}

func jsonObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 1 && raw[0] == '{'
}

// DecodeStrict accepts one object and rejects unknown fields, null objects and trailing values.
func DecodeStrict(raw []byte, target any) error {
	if !jsonObject(raw) {
		return errors.New("expected JSON object")
	}
	if err := validateJSON(raw, 512); err != nil {
		return err
	}
	return jsonv2.Unmarshal(raw, target, jsonv2.RejectUnknownMembers(true))
}

type Call struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type Result struct {
	ID     string          `json:"id"`
	Status int             `json:"http_status,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type Response struct {
	Messages []string `json:"messages,omitempty"`
	Calls    []Call   `json:"tool_calls,omitempty"`
	Finished bool     `json:"finished,omitempty"`
	Cost     *int64   `json:"cost_minor,omitempty"`
}

type Input struct {
	Message *string  `json:"message,omitempty"`
	Results []Result `json:"tool_results,omitempty"`
}

type Agent interface {
	Exchange(context.Context, Input) (Response, error)
	Close(context.Context) error
}

type AgentFactory interface {
	Open(context.Context, Scope, AgentConfig, string) (Agent, error)
}

type SimulationInput struct {
	Scope                    Scope
	Seed                     int64
	Definition               json.RawMessage
	Actor, Goal, Clock, Zone string
	Facts                    json.RawMessage
	Modality                 string
	VoiceActions             json.RawMessage
}

type Action struct {
	Text     string
	Finished bool
}

type Simulator interface {
	Next(context.Context, []string) (Action, error)
	Close(context.Context) error
}

type SimulatorFactory interface {
	Open(context.Context, SimulationInput) (Simulator, error)
}

type Setup struct {
	Template, TemplateVersion   string
	Definition, Fixture, Faults json.RawMessage
}

type Workspace interface {
	Call(context.Context, Call) Result
	Finalize(context.Context) (snapshot, audit Ref, err error)
}

type Environment interface {
	Open(context.Context, Scope, Setup) (Workspace, error)
}

type Ref struct {
	ID   string `json:"artifact_id"`
	SHA  string `json:"sha256"`
	Size int64  `json:"size_bytes,string"`
	MIME string `json:"mime"`
	Kind string `json:"kind"`
}

func (r Ref) Valid(kind string) bool {
	return IDValid(r.ID) && HashValid(r.SHA) && r.Size >= 0 && r.Size <= 512<<20 &&
		len(r.MIME) > 0 && len(r.MIME) <= 200 && r.Kind == kind
}

type Access struct {
	Scope         Scope
	Worker, Lease string
}

type Evidence interface {
	Upload(context.Context, Access, Blob) (Ref, error)
	Resolve(context.Context, Access, Ref) error
}
