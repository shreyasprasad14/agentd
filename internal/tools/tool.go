// Package tools defines the Tool interface and the per-process registry the
// agent loop dispatches through. Builtins live in the builtin subpackage; the
// sandboxed python tool (M2), retrieval tools (M3), and the MCP adapter (M6)
// all implement the same interface.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// TrustTier says how much a tool's code is trusted, which decides where it
// runs. Builtin runs in-process, Sandboxed runs in a container, External is
// an MCP server or similar out-of-process peer.
type TrustTier int

const (
	Builtin TrustTier = iota
	Sandboxed
	External
)

func (t TrustTier) String() string {
	switch t {
	case Builtin:
		return "builtin"
	case Sandboxed:
		return "sandboxed"
	case External:
		return "external"
	}
	return "unknown"
}

// MarshalJSON renders the tier by name in API listings.
func (t TrustTier) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

// UnmarshalJSON parses the name form.
func (t *TrustTier) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	for _, tier := range []TrustTier{Builtin, Sandboxed, External} {
		if tier.String() == s {
			*t = tier
			return nil
		}
	}
	return fmt.Errorf("unknown trust tier %q", s)
}

// Invocation is one tool call. RunID and Seq together form the idempotency key
// for this call: a tool with real side effects should key its own dedup on
// them, because a crash between execution and the result commit re-invokes
// it with the same key.
type Invocation struct {
	RunID uuid.UUID
	Seq   int32
	Args  json.RawMessage
}

// Cost is model spend a tool incurred inside itself. It is kept here rather
// than in the model package so tools does not depend on model: the loop
// converts it when it writes tool_succeeded (ADR-23).
type Cost struct {
	MicroUSD     int64 `json:"micro_usd,omitempty"`
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// Model names the backend that was billed, for the event log.
	Model string `json:"model,omitempty"`
}

// Add sums two costs. The first non-empty Model wins, since a tool that
// calls one model repeatedly is the case worth naming.
func (c Cost) Add(o Cost) Cost {
	sum := Cost{
		MicroUSD:     c.MicroUSD + o.MicroUSD,
		InputTokens:  c.InputTokens + o.InputTokens,
		OutputTokens: c.OutputTokens + o.OutputTokens,
		Model:        c.Model,
	}
	if sum.Model == "" {
		sum.Model = o.Model
	}
	return sum
}

// IsZero reports whether the tool spent nothing, which is every builtin. A
// Model name with no tokens behind it is still nothing spent: there are no
// counters to bump, and naming a backend that was never billed would put a
// cost on tool_succeeded that no invoice agrees with.
func (c Cost) IsZero() bool {
	return c.MicroUSD == 0 && c.InputTokens == 0 && c.OutputTokens == 0
}

// Result is what a tool returns. Content goes back to the model, wrapped in
// the data envelope by the loop.
type Result struct {
	Content json.RawMessage
	// ExitCode is meaningful for sandboxed tools; builtins report 0.
	ExitCode int
	// Terminal marks a tool whose successful call ends the run (finish).
	Terminal bool
	// Cost is model spend the tool incurred inside itself. The loop adds it
	// to the run's counters in the same transaction as tool_succeeded, so a
	// tool that calls a model is bounded by the run's budget rather than
	// invisible to it.
	Cost Cost
}

// Tool is the one interface every callable capability implements.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema for Args. The registry validates every
	// invocation against it before Invoke is called.
	Schema() json.RawMessage
	TrustTier() TrustTier
	Invoke(ctx context.Context, inv Invocation) (Result, error)
}
