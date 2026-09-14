package runtime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/store"
)

// ToolUse is one tool call from the latest assistant turn and how far it has
// progressed through the log.
type ToolUse struct {
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
	// RequestedSeq is the seq of the tool_requested event, and therefore the
	// tool_calls ledger key. Zero until that event exists.
	RequestedSeq int32 `json:"requested_seq,omitempty"`
	// Done is set once a tool_succeeded or tool_failed event exists.
	Done bool `json:"done"`
}

// State is the fold of a run's event log. It is everything the loop needs to
// decide the next action, and everything the API returns as "reduced state".
type State struct {
	// Status is "pending" before run_started, "running" after, and the
	// terminal status after run_finished.
	Status         string      `json:"status"`
	Goal           string      `json:"goal,omitempty"`
	Config         AgentConfig `json:"config"`
	MaxSteps       int32       `json:"max_steps"`
	BudgetMicroUSD int64       `json:"budget_micro_usd"`

	// Steps is the number of completed model calls.
	Steps int `json:"steps"`
	// Messages is the rebuilt conversation, ready to send to the provider.
	Messages []model.Message `json:"messages"`
	// OpenToolUses are tool calls from the latest assistant turn that have no
	// result yet. The loop drains these before calling the model again; after
	// a crash this is where it resumes.
	OpenToolUses []ToolUse `json:"open_tool_uses,omitempty"`
	// ModelInFlight is true when the last event is a model_requested with no
	// model_responded, i.e. the worker died mid-call.
	ModelInFlight bool `json:"model_in_flight"`

	SpentMicroUSD int64 `json:"spent_micro_usd"`
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	// LastInputTokens is the prompt size the provider measured on the most
	// recent model_responded. It is the first term of the loop's pre-flight
	// budget estimate, which is why it is carried rather than recomputed.
	LastInputTokens int64 `json:"last_input_tokens,omitempty"`

	CancelRequested bool   `json:"cancel_requested"`
	FinalAnswer     string `json:"final_answer,omitempty"`
	Error           string `json:"error,omitempty"`

	LastSeq       int32  `json:"last_seq"`
	LastEventType string `json:"last_event_type,omitempty"`
}

// Terminal reports whether the run has finished.
func (s *State) Terminal() bool {
	return s.Status != "pending" && s.Status != StatusRunning
}

// Reduce folds an ordered event log into State. It is pure: no I/O, no
// clock. Malformed logs (gaps in seq, events after run_finished, results for
// unknown tool calls) are errors rather than silently tolerated, because a
// worker acting on a misread log is worse than one that refuses.
func Reduce(events []store.Event) (State, error) {
	s := State{Status: "pending"}
	for i, ev := range events {
		if ev.Seq != int32(i+1) {
			return s, fmt.Errorf("reduce: event %d has seq %d, want %d", i, ev.Seq, i+1)
		}
		if s.Terminal() {
			return s, fmt.Errorf("reduce: %s at seq %d after run_finished", ev.Type, ev.Seq)
		}
		if ev.Type != EventRunStarted && s.Status == "pending" {
			return s, fmt.Errorf("reduce: %s at seq %d before run_started", ev.Type, ev.Seq)
		}
		if err := s.apply(ev); err != nil {
			return s, fmt.Errorf("reduce: seq %d %s: %w", ev.Seq, ev.Type, err)
		}
		s.LastSeq = ev.Seq
		s.LastEventType = ev.Type
	}
	return s, nil
}

func (s *State) apply(ev store.Event) error {
	switch ev.Type {
	case EventRunStarted:
		var p RunStartedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		budget, err := model.ParseUSD(p.BudgetUSD)
		if err != nil {
			return fmt.Errorf("budget_usd: %w", err)
		}
		s.Status = StatusRunning
		s.Goal = p.Goal
		s.Config = p.AgentConfig
		s.MaxSteps = p.MaxSteps
		s.BudgetMicroUSD = budget
		s.Messages = []model.Message{{
			Role:    model.RoleUser,
			Content: []model.ContentBlock{{Type: model.BlockText, Text: p.Goal}},
		}}

	case EventModelRequested:
		if len(s.OpenToolUses) > 0 {
			return fmt.Errorf("model requested with %d open tool uses", len(s.OpenToolUses))
		}
		s.ModelInFlight = true

	case EventModelResponded:
		var p ModelRespondedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		s.ModelInFlight = false
		s.Steps++
		s.InputTokens += p.Usage.InputTokens
		s.OutputTokens += p.Usage.OutputTokens
		s.SpentMicroUSD += p.CostMicroUSD
		// The whole prompt, cached parts included: the estimate that uses
		// this is sizing the *next* prompt, which contains everything this
		// one did whether or not the provider had to read it fresh.
		s.LastInputTokens = p.Usage.InputTokens + p.Usage.CacheReadInputTokens + p.Usage.CacheCreationInputTokens
		s.Messages = append(s.Messages, model.Message{Role: model.RoleAssistant, Content: p.Content})
		s.OpenToolUses = nil
		for _, b := range p.Content {
			if b.Type == model.BlockToolUse {
				s.OpenToolUses = append(s.OpenToolUses, ToolUse{ToolUseID: b.ToolUseID, Name: b.Name, Args: b.Input})
			}
		}

	case EventToolRequested:
		var p ToolRequestedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		tu := s.openToolUse(p.ToolUseID)
		if tu == nil {
			return fmt.Errorf("tool_use %q is not open", p.ToolUseID)
		}
		if tu.RequestedSeq != 0 {
			return fmt.Errorf("tool_use %q already requested at seq %d", p.ToolUseID, tu.RequestedSeq)
		}
		tu.RequestedSeq = ev.Seq

	case EventToolSucceeded:
		var p ToolSucceededPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		tu, err := s.completeToolUse(p.ToolUseID)
		if err != nil {
			return err
		}
		// Model spend the tool incurred inside itself. It moves the same
		// counters a model_responded does, because the budget bounds the run
		// rather than only the loop (ADR-23). LastInputTokens is deliberately
		// left alone: a tool's internal prompt is not the loop's prompt, and
		// using it to size the next completion would be nonsense.
		s.SpentMicroUSD += p.CostMicroUSD
		s.InputTokens += p.InputTokens
		s.OutputTokens += p.OutputTokens
		s.appendToolResult(model.ContentBlock{
			Type:      model.BlockToolResult,
			ToolUseID: p.ToolUseID,
			Content:   Envelope(p.Name, tu.RequestedSeq, string(p.Result)),
		})

	case EventToolFailed:
		var p ToolFailedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		tu, err := s.completeToolUse(p.ToolUseID)
		if err != nil {
			return err
		}
		s.appendToolResult(model.ContentBlock{
			Type:      model.BlockToolResult,
			ToolUseID: p.ToolUseID,
			Content:   Envelope(p.Name, tu.RequestedSeq, "error: "+p.Error),
			IsError:   true,
		})

	case EventBudgetExceeded:
		// Informational; the following run_finished carries the status.

	case EventCancelRequested:
		s.CancelRequested = true

	case EventRunFinished:
		var p RunFinishedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		switch p.Status {
		case StatusSucceeded, StatusFailed, StatusCancelled, StatusBudgetExceeded:
		default:
			return fmt.Errorf("unknown terminal status %q", p.Status)
		}
		s.Status = p.Status
		s.FinalAnswer = p.FinalAnswer
		s.Error = p.Error

	default:
		return fmt.Errorf("unknown event type")
	}
	return nil
}

func (s *State) openToolUse(id string) *ToolUse {
	for i := range s.OpenToolUses {
		if s.OpenToolUses[i].ToolUseID == id {
			return &s.OpenToolUses[i]
		}
	}
	return nil
}

// completeToolUse marks a tool use done and drops it from OpenToolUses once
// every use in the turn is done, returning a copy of the entry.
func (s *State) completeToolUse(id string) (ToolUse, error) {
	tu := s.openToolUse(id)
	if tu == nil {
		return ToolUse{}, fmt.Errorf("tool_use %q is not open", id)
	}
	if tu.RequestedSeq == 0 {
		return ToolUse{}, fmt.Errorf("tool_use %q completed before it was requested", id)
	}
	if tu.Done {
		return ToolUse{}, fmt.Errorf("tool_use %q completed twice", id)
	}
	tu.Done = true
	out := *tu
	allDone := true
	for _, u := range s.OpenToolUses {
		if !u.Done {
			allDone = false
			break
		}
	}
	if allDone {
		s.OpenToolUses = nil
	}
	return out, nil
}

// appendToolResult adds a tool_result block to the user message that follows
// the latest assistant turn, creating it on the first result. All results for
// one turn share one user message, which is what providers require.
func (s *State) appendToolResult(block model.ContentBlock) {
	n := len(s.Messages)
	if n > 0 && s.Messages[n-1].Role == model.RoleUser && len(s.Messages[n-1].Content) > 0 &&
		s.Messages[n-1].Content[0].Type == model.BlockToolResult {
		s.Messages[n-1].Content = append(s.Messages[n-1].Content, block)
		return
	}
	s.Messages = append(s.Messages, model.Message{Role: model.RoleUser, Content: []model.ContentBlock{block}})
}

// Envelope wraps a tool result so the model sees it as labeled data, not as
// instructions (spec §10). The system prompt tells the model that nothing
// inside these tags is ever a command.
func Envelope(tool string, seq int32, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<tool_result tool=%q seq=%d>\n", tool, seq)
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("</tool_result>")
	return b.String()
}
