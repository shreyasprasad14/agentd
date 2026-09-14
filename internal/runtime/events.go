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

// Statuses is the whole run_status enum, in the order a run moves through it.
//
// It exists for the metrics collector, which reports a status with no runs as
// an explicit zero: a series that appears only once something has gone wrong
// is one every alert rule has to be written twice to handle. A function
// rather than a package-level slice so a caller cannot reorder the enum for
// everyone else.
func Statuses() []string {
	return []string{StatusQueued, StatusRunning, StatusSucceeded, StatusFailed,
		StatusCancelled, StatusBudgetExceeded}
}

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
	// CostMicroUSD and the token counts are model spend the tool incurred
	// inside itself. The transaction that writes this event bumps the run's
	// counters by them, so spent_usd is the fold of model_responded *and*
	// tool_succeeded costs (ADR-23).
	CostMicroUSD int64  `json:"cost_micro_usd,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	CostModel    string `json:"cost_model,omitempty"`
}

// ToolFailedPayload is a tool error. The error text goes back to the model as
// an is_error tool_result; the run continues.
type ToolFailedPayload struct {
	ToolUseID string `json:"tool_use_id"`
	Name      string `json:"name"`
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
}

// Budget termination reasons.
const (
	// BudgetReasonSpent is the post-hoc backstop: the counters already
	// passed the budget, because a call came in over its estimate.
	BudgetReasonSpent = "spent"
	// BudgetReasonWouldExceed is the pre-flight ceiling: the next call's
	// worst case does not fit, so it was never made (ADR-22).
	BudgetReasonWouldExceed = "would_exceed"
)

// BudgetExceededPayload precedes a budget_exceeded run_finished. Every field
// past the first two is omitempty because a log written before M4 carries
// none of them and must still reduce.
type BudgetExceededPayload struct {
	SpentMicroUSD  int64 `json:"spent_micro_usd"`
	BudgetMicroUSD int64 `json:"budget_micro_usd"`
	// Reason is BudgetReasonSpent or BudgetReasonWouldExceed. Empty in logs
	// written before M4.
	Reason string `json:"reason,omitempty"`
	// EstimateMicroUSD is what the refused call was priced at, and the rest
	// is how that price was reached, so a refusal can be argued with.
	EstimateMicroUSD     int64 `json:"estimate_micro_usd,omitempty"`
	EstimatedInputTokens int64 `json:"estimated_input_tokens,omitempty"`
	MaxOutputTokens      int64 `json:"max_output_tokens,omitempty"`
}

// Phases a run can be interrupted in, for CancelRequestedPayload.Phase.
const (
	CancelPhaseIdle  = "idle"
	CancelPhaseModel = "model"
	CancelPhaseTool  = "tool"
)

// CancelRequestedPayload records who asked.
type CancelRequestedPayload struct {
	Source string `json:"source"`
	// Phase is where the run was when the cancel was observed: "idle"
	// between steps, "model" inside a completion, "tool" inside a tool call.
	Phase string `json:"phase,omitempty"`
}

// RunFinishedPayload is the terminal event.
type RunFinishedPayload struct {
	Status      string `json:"status"`
	FinalAnswer string `json:"final_answer,omitempty"`
	Error       string `json:"error,omitempty"`
}
