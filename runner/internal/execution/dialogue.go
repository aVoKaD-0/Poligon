package execution

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type dialogue struct {
	engine                                 *Engine
	rec                                    *recorder
	plan                                   Plan
	turns, toolCalls, replies, costReports int
	cost                                   int64
	incomplete                             bool
	callIDs                                map[string]struct{}
}

func (d *dialogue) interact(ctx, lease context.Context, credential string) (Workspace, string, failure) {
	p := d.plan
	var env Workspace
	var agent Agent
	var simulator Simulator
	var err error
	stage := "environment.open"
	setup := Setup{p.Scenario.Template, p.Scenario.TemplateVersion, p.Environment, p.Scenario.Fixture, p.Scenario.Faults}
	if CheckLease(ctx) == nil {
		env, err = d.engine.Environment.Open(ctx, p.Scope, setup)
	}
	if err == nil && CheckLease(ctx) == nil {
		stage = "agent.open"
		agent, err = d.engine.Agents.Open(ctx, p.Scope, p.Agent, credential)
	}
	if err == nil && CheckLease(ctx) == nil {
		stage = "simulator.open"
		simulator, err = d.engine.Simulators.Open(ctx, SimulationInput{
			p.Scope, *p.Seed, p.Simulator, p.Scenario.Actor, p.Scenario.Goal, p.Scenario.Clock, p.Scenario.Zone,
			p.Scenario.Facts, p.Scenario.Modality, p.Scenario.VoiceActions,
		})
	}
	var reason string
	var issue failure
	if err != nil || CheckLease(ctx) != nil {
		reason, issue = d.outcome(ctx, stage, err)
	} else {
		reason, issue = d.loop(ctx, agent, simulator, env)
	}
	closeCtx, cancel := context.WithTimeout(lease, 2*time.Second)
	defer cancel()
	for _, c := range []struct {
		name  string
		close func(context.Context) error
	}{
		{"agent.close", closeAgent(agent)}, {"simulator.close", closeSimulator(simulator)},
	} {
		if c.close == nil {
			continue
		}
		if err := c.close(closeCtx); err != nil {
			_ = d.rec.append(lease, d.rec.conversation, "session.error", map[string]string{"stage": c.name, "error_code": "TRANSPORT_ERROR"})
			if issue.code == "" && reason != "agent_protocol_error" && reason != "cancelled" {
				reason, issue = "infrastructure_error", failure{"TRANSPORT_ERROR", true}
			}
		}
	}
	return env, reason, issue
}

func closeAgent(a Agent) func(context.Context) error {
	if a == nil {
		return nil
	}
	return a.Close
}
func closeSimulator(s Simulator) func(context.Context) error {
	if s == nil {
		return nil
	}
	return s.Close
}

func (d *dialogue) loop(ctx context.Context, agent Agent, sim Simulator, env Workspace) (string, failure) {
	l := d.plan.Limits
	var last []string
	for d.turns < l.Turns {
		if err := CheckLease(ctx); err != nil {
			return d.outcome(ctx, "simulator.next", err)
		}
		action, err := sim.Next(ctx, last)
		if err != nil || ctx.Err() != nil {
			return d.outcome(ctx, "simulator.next", err)
		}
		if action.Finished && action.Text == "" {
			return "client_finished", failure{}
		}
		if strings.TrimSpace(action.Text) == "" || len(action.Text) > 24<<10 || action.Finished ||
			!utf8.ValidString(action.Text) || strings.IndexByte(action.Text, 0) >= 0 {
			return d.outcome(ctx, "simulator.next", ErrDependency)
		}
		d.turns++
		if err := d.rec.append(ctx, d.rec.conversation, "client.message", map[string]any{"text": action.Text, "turn": d.turns}); err != nil {
			return "infrastructure_error", failure{journalError(err), !errors.Is(err, ErrLimit)}
		}
		input := Input{Message: &action.Text}
		last = nil
		for {
			if err := CheckLease(ctx); err != nil {
				return d.outcome(ctx, "agent.exchange", err)
			}
			request, span := otel.Tracer("poligon/runner").Start(ctx, "agent.exchange")
			start := time.Now()
			reply, err := agent.Exchange(request, input)
			elapsed := time.Since(start)
			span.End()
			d.engine.Metrics.AgentRequests.Add(1)
			d.engine.Metrics.AgentNanos.Add(uint64(elapsed))
			if err != nil || ctx.Err() != nil {
				d.incomplete = true
				return d.outcome(ctx, "agent.exchange", err)
			}
			if !validResponse(reply) || (reply.Cost != nil && *reply.Cost > math.MaxInt64-d.cost) {
				d.incomplete = true
				return d.outcome(ctx, "agent.exchange", ErrProtocol)
			}
			d.replies++
			if reply.Cost != nil {
				d.costReports++
				d.cost += *reply.Cost
			}
			if err := d.rec.append(ctx, d.rec.conversation, "agent.message", map[string]any{
				"messages": reply.Messages, "finished": reply.Finished, "cost_minor": reply.Cost,
				"duration_ms": elapsed.Milliseconds(), "started_at": start.UTC(), "turn": d.turns,
			}); err != nil {
				return "infrastructure_error", failure{journalError(err), !errors.Is(err, ErrLimit)}
			}
			if l.Budget != nil && d.costReports > 0 && d.cost > *l.Budget {
				return "budget_exceeded", failure{}
			}
			if reply.Finished {
				return "completed", failure{}
			}
			if len(reply.Calls) == 0 {
				if d.toolCalls >= l.Tools {
					return "max_tool_calls", failure{}
				}
				// Keep only the final response in the previous turn. It is bounded by validResponse.
				last = reply.Messages
				break
			}
			if err := d.validateCalls(reply.Calls); err != nil {
				return d.outcome(ctx, "agent.tool_calls", err)
			}
			accepted := min(len(reply.Calls), l.Tools-d.toolCalls)
			if bounded, ok := agent.(interface{ CheckToolResults([]Call) error }); ok {
				if err := bounded.CheckToolResults(reply.Calls[:accepted]); err != nil {
					return d.outcome(ctx, "agent.tool_calls", err)
				}
			}
			input = Input{Results: make([]Result, 0, accepted)}
			for _, call := range reply.Calls {
				if err := CheckLease(ctx); err != nil {
					return d.outcome(ctx, "tool.call", err)
				}
				if d.toolCalls >= l.Tools {
					if err := d.rec.append(ctx, d.rec.tools, "tool.skipped", map[string]any{"call": call, "reason": "max_tool_calls"}); err != nil {
						return "infrastructure_error", failure{journalError(err), !errors.Is(err, ErrLimit)}
					}
					continue
				}
				if err := d.rec.append(ctx, d.rec.tools, "tool.request", call); err != nil {
					return "infrastructure_error", failure{journalError(err), !errors.Is(err, ErrLimit)}
				}
				if err := CheckLease(ctx); err != nil {
					return d.outcome(ctx, "tool.call", err)
				}
				request, span := otel.Tracer("poligon/runner").Start(ctx, "environment.tool")
				t := time.Now()
				result := env.Call(request, call)
				elapsed := time.Since(t)
				span.SetAttributes(attribute.Int("http.response.status_code", result.Status))
				span.End()
				d.toolCalls++
				d.engine.Metrics.ToolCalls.Add(1)
				if err := d.rec.append(ctx, d.rec.tools, "tool.result", map[string]any{
					"result": result, "duration_ms": elapsed.Milliseconds(), "started_at": t.UTC(),
				}); err != nil {
					return "infrastructure_error", failure{journalError(err), !errors.Is(err, ErrLimit)}
				}
				input.Results = append(input.Results, result)
			}
			if len(input.Results) == 0 {
				return "max_tool_calls", failure{}
			}
		}
	}
	return "max_turns", failure{}
}

func validResponse(r Response) bool {
	if len(r.Messages) > 16 || len(r.Calls) > 16 || (r.Finished && len(r.Calls) > 0) ||
		(!r.Finished && len(r.Messages) == 0 && len(r.Calls) == 0) || (r.Cost != nil && *r.Cost < 0) {
		return false
	}
	n := 0
	for _, m := range r.Messages {
		n += len(m)
	}
	return n <= 24<<10
}

func (d *dialogue) validateCalls(calls []Call) error {
	for _, c := range calls {
		if c.ID == "" || len(c.ID) > 128 || c.Name == "" || len(c.Name) > 128 || len(c.Arguments) > 16<<10 || len(c.IdempotencyKey) > 256 {
			return ErrProtocol
		}
		if _, found := d.callIDs[c.ID]; found {
			return ErrProtocol
		}
		d.callIDs[c.ID] = struct{}{}
	}
	return nil
}

func (d *dialogue) outcome(ctx context.Context, stage string, err error) (string, failure) {
	switch context.Cause(ctx) {
	case errCancelled:
		return "cancelled", failure{}
	case errSessionDeadline:
		return "session_deadline", failure{}
	}
	issue := failure{"DEPENDENCY_UNAVAILABLE", !errors.Is(err, ErrUnsupported)}
	reason := "infrastructure_error"
	if errors.Is(err, ErrMessageLimit) {
		issue = failure{"MESSAGE_LIMIT_EXCEEDED", false}
	} else if errors.Is(err, ErrProtocol) {
		issue = failure{"AGENT_PROTOCOL_ERROR", false}
		reason = "agent_protocol_error"
	} else if strings.HasPrefix(stage, "agent.") && !errors.Is(err, ErrUnsupported) {
		issue = failure{"AGENT_TRANSPORT_ERROR", false}
	}
	_ = d.rec.append(ctx, d.rec.conversation, "session.error", map[string]string{"stage": stage, "error_code": issue.code})
	// A protocol defect is evaluable if the mandatory evidence can be finalized.
	if reason == "agent_protocol_error" {
		issue = failure{}
	}
	return reason, issue
}
