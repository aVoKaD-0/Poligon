package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/poligon/runner/internal/execution"
)

type Agents struct {
	Client            *http.Client
	Hosts             map[string]bool
	Local, EnableHTTP bool
	Timeout           time.Duration
	Limit             int64
}

func (a *Agents) agentEndpoint(raw string) (*url.URL, error) {
	u, err := Endpoint(raw, a.Local)
	if err != nil || u.RawQuery != "" || u.ForceQuery || !a.Hosts[strings.ToLower(u.Hostname())] {
		return nil, execution.ErrProtocol
	}
	return u, nil
}

func (a *Agents) Open(ctx context.Context, _ execution.Scope, cfg execution.AgentConfig, token string) (execution.Agent, error) {
	if cfg.Adapter != "http-json-v1" || !a.EnableHTTP {
		return nil, execution.ErrUnsupported
	}
	endpoint, err := a.agentEndpoint(cfg.Endpoint)
	if err != nil || !headerValue(token) {
		return nil, execution.ErrProtocol
	}
	token = strings.Trim(token, " ")
	timeout := a.Timeout
	if cfg.TimeoutMS > 0 {
		timeout = min(timeout, time.Duration(cfg.TimeoutMS)*time.Millisecond)
	}
	session := &httpAgent{a.Client, endpoint.String(), uuid.NewString(), token, timeout, a.Limit}
	if _, err := session.exchange(ctx, "open", nil); err != nil {
		return nil, err
	}
	return session, nil
}

type httpAgent struct {
	client         *http.Client
	url, id, token string
	timeout        time.Duration
	limit          int64
}

type wireRequest struct {
	Version int              `json:"schema_version"`
	Session string           `json:"session_id"`
	Action  string           `json:"action"`
	Input   *execution.Input `json:"input,omitempty"`
}

type wireResponse struct {
	Version int        `json:"schema_version"`
	Session string     `json:"session_id"`
	Action  string     `json:"action"`
	Reply   *wireReply `json:"reply,omitempty"`
}

type wireReply struct {
	execution.Response
	Messages []*string         `json:"messages,omitempty"`
	Calls    []*execution.Call `json:"tool_calls,omitempty"`
}

func (r wireReply) response() (execution.Response, error) {
	reply := r.Response
	if r.Messages != nil {
		reply.Messages = make([]string, len(r.Messages))
		for i, message := range r.Messages {
			if message == nil {
				return execution.Response{}, execution.ErrProtocol
			}
			reply.Messages[i] = *message
		}
	}
	if r.Calls != nil {
		reply.Calls = make([]execution.Call, len(r.Calls))
		for i, call := range r.Calls {
			if call == nil {
				return execution.Response{}, execution.ErrProtocol
			}
			reply.Calls[i] = *call
		}
	}
	return reply, nil
}

func (a *httpAgent) Exchange(ctx context.Context, input execution.Input) (execution.Response, error) {
	return a.exchange(ctx, "exchange", &input)
}

func (a *httpAgent) Close(ctx context.Context) error {
	_, err := a.exchange(ctx, "close", nil)
	return err
}

func (a *httpAgent) CheckToolResults(calls []execution.Call) error {
	results := make([]execution.Result, len(calls))
	for i, call := range calls {
		// Reserve room for a status and the longest CRM adapter error even
		// when none of the response bodies fit the wire limit.
		results[i] = execution.Result{ID: call.ID, Status: 999, Error: "invalid_environment_response"}
	}
	raw, err := execution.MarshalJSON(wireRequest{1, a.id, "exchange", &execution.Input{Results: results}})
	if err == nil && int64(len(raw)) > a.limit {
		return execution.ErrMessageLimit
	}
	return err
}

func (a *httpAgent) fitToolResults(input execution.Input, size int64) ([]byte, error) {
	results := slices.Clone(input.Results)
	type omission struct {
		index, saved int
		result       execution.Result
	}
	var candidates []omission
	for i, result := range results {
		if len(result.Body) == 0 {
			continue
		}
		full, err := execution.MarshalJSON(result)
		if err != nil {
			return nil, err
		}
		result.Body = nil
		if result.Error == "" {
			result.Error = "response_too_large"
		}
		short, err := execution.MarshalJSON(result)
		if err != nil {
			return nil, err
		}
		if saved := len(full) - len(short); saved > 0 {
			candidates = append(candidates, omission{i, saved, result})
		}
	}
	// Drop the fewest bodies needed to fit. Keep the original result order,
	// identifiers, statuses, and any bodies that still fit.
	slices.SortStableFunc(candidates, func(a, b omission) int { return b.saved - a.saved })
	for _, candidate := range candidates {
		results[candidate.index] = candidate.result
		size -= int64(candidate.saved)
		if size <= a.limit {
			input.Results = results
			return execution.MarshalJSON(wireRequest{1, a.id, "exchange", &input})
		}
	}
	return nil, execution.ErrMessageLimit
}

func (a *httpAgent) exchange(ctx context.Context, action string, input *execution.Input) (execution.Response, error) {
	var empty execution.Response
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	raw, err := execution.MarshalJSON(wireRequest{1, a.id, action, input})
	if err != nil {
		return empty, err
	}
	if int64(len(raw)) > a.limit {
		if input == nil || len(input.Results) == 0 {
			return empty, execution.ErrMessageLimit
		}
		raw, err = a.fitToolResults(*input, int64(len(raw)))
		if err != nil {
			return empty, err
		}
	}
	h := make(http.Header)
	if a.token != "" {
		h.Set("Authorization", "Bearer "+a.token)
	}
	status, body, err := send(ctx, a.client, http.MethodPost, a.url, raw, h, a.limit)
	if err != nil {
		if err == errSize {
			return empty, execution.ErrProtocol
		}
		return empty, err
	}
	if status != http.StatusOK {
		return empty, fmt.Errorf("%w: HTTP %d", execution.ErrProtocol, status)
	}
	var out wireResponse
	if execution.DecodeStrict(body, &out) != nil || out.Version != 1 || out.Session != a.id || out.Action != action ||
		(out.Reply != nil) != (action == "exchange") {
		return empty, execution.ErrProtocol
	}
	if out.Reply == nil {
		return empty, nil
	}
	return out.Reply.response()
}
