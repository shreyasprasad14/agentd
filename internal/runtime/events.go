package runtime

import (
	"encoding/json"

	"github.com/shreyasprasad/agentd/internal/model"
)

// Event types written to run_events. The log is the source of truth for run
// state; everything else in the runs table is a derived counter.
const (
	EventRunStarted      = "run_started"
	EventModelRequested  = "model_requested"
	EventModelResponded  = "model_responded"
	EventToolRequested   = "tool_requested"
	EventToolSucceeded   = "tool_succeeded"
	EventToolFailed      = "tool_failed"
	EventBudgetExceeded  = "budget_exceeded"
	EventCancelRequested = "cancel_requested"
	EventRunFinished     = "run_finished"
)

// IsTerminal reports whether no further events can follow this one. The SSE
// handler closes a stream once one arrives.
func IsTerminal(eventType string) bool {
	return eventType == EventRunFinished
}

// Terminal run statuses, matching the run_status enum.
const (
	StatusQueued         = "queued"
	StatusRunning        = "running"
	StatusSucceeded      = "succeeded"
	StatusFailed         = "failed"
	StatusCancelled      = "cancelled"
	StatusBudgetExceeded = "budget_exceeded"
)

// AgentConfig is the per-run configuration stored in runs.agent_config and
// snapshotted into run_started. Tools is the allowlist, fixed at submission.
type AgentConfig struct {
	Model        string   `json:"model,omitempty"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	Tools        []string `json:"tools"`
	MaxTokens    int      `json:"max_tokens,omitempty"`
	// ToolDelayMS artificially slows every tool call. It exists so the crash
	// demo and the resilience evals can kill a worker mid-tool-call on
	// purpose; production runs leave it zero.
	ToolDelayMS int `json:"tool_delay_ms,omitempty"`
}

// RunStartedPayload is the first event of every run.
type RunStartedPayload struct {
	Goal        string      `json:"goal"`
	AgentConfig AgentConfig `json:"agent_config"`
	MaxSteps    int32       `json:"max_steps"`
	BudgetUSD   string      `json:"budget_usd"`
	Worker      string      `json:"worker"`
}

// ModelParams records what the model was asked with, for cassette matching.
type ModelParams struct {
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	Tools       []string `json:"tools"`
}

// ModelRequestedPayload is written before every model call. A dangling one
// (no model_responded after it) means the worker died mid-call; the loop
// simply re-issues the call.
type ModelRequestedPayload struct {
	Step           int         `json:"step"`
	Model          string      `json:"model"`
	MessagesSHA256 string      `json:"messages_sha256"`
	Params         ModelParams `json:"params"`
}

// ModelRespondedPayload is the model's answer and its usage. The transaction
// that writes it also bumps the run's token and spend counters.
type ModelRespondedPayload struct {
	Step         int                  `json:"step"`
	Provider     string               `json:"provider"`
	Model        string               `json:"model"`
	Content      []model.ContentBlock `json:"content"`
	StopReason   string               `json:"stop_reason"`
	Usage        model.Usage          `json:"usage"`
	CostMicroUSD int64                `json:"cost_micro_usd"`
}

// ToolRequestedPayload is written, in the same transaction as the tool_calls
// ledger row, before a tool runs. Its seq is the idempotency key.
type ToolRequestedPayload struct {
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
}

// ToolSucceededPayload carries a tool result. Replayed is true when the result
// was read back from the ledger after a resume instead of being re-executed.
type ToolSucceededPayload struct {
	ToolUseID  string          `json:"tool_use_id"`
	Name       string          `json:"name"`
	Result     json.RawMessage `json:"result"`
	DurationMS int64           `json:"duration_ms"`
	ExitCode   int             `json:"exit_code"`
	Replayed   bool            `json:"replayed,omitempty"`
}

// ToolFailedPayload is a tool error. The error text goes back to the model as
// an is_error tool_result; the run continues.
type ToolFailedPayload struct {
	ToolUseID string `json:"tool_use_id"`
	Name      string `json:"name"`
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
}

// BudgetExceededPayload precedes a budget_exceeded run_finished.
type BudgetExceededPayload struct {
	SpentMicroUSD  int64 `json:"spent_micro_usd"`
	BudgetMicroUSD int64 `json:"budget_micro_usd"`
}

// CancelRequestedPayload records who asked.
type CancelRequestedPayload struct {
	Source string `json:"source"`
}

// RunFinishedPayload is the terminal event.
type RunFinishedPayload struct {
	Status      string `json:"status"`
	FinalAnswer string `json:"final_answer,omitempty"`
	Error       string `json:"error,omitempty"`
}
