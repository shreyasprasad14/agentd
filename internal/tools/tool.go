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

// Result is what a tool returns. Content goes back to the model, wrapped in
// the data envelope by the loop.
type Result struct {
	Content json.RawMessage
	// ExitCode is meaningful for sandboxed tools; builtins report 0.
	ExitCode int
	// Terminal marks a tool whose successful call ends the run (finish).
	Terminal bool
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
