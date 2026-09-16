package runtime_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/tools"
	"github.com/shreyasprasad/agentd/internal/tools/builtin"
	"github.com/shreyasprasad/agentd/internal/tools/mcp"
	"github.com/shreyasprasad/agentd/internal/tools/mcp/testserver"
)

// mcpFixture starts the in-repo MCP server over HTTP and returns the tools it
// advertises. HTTP rather than stdio on purpose: the adapter's own tests cover
// the subprocess path, and what this file is testing is the loop, so an
// in-process server keeps the test hermetic and free of a child to reap.
func mcpFixture(t *testing.T, opts testserver.Options) []tools.Tool {
	t.Helper()
	srv := httptest.NewServer(testserver.Handler(opts))
	t.Cleanup(srv.Close)

	ts, closer, err := mcp.Connect(context.Background(), mcp.Config{Servers: []mcp.ServerConfig{{
		Name:      "legal",
		Transport: mcp.TransportHTTP,
		URL:       srv.URL,
	}}}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closer.Close() })
	return ts
}

// toolNames is the registered name of each tool, for allowlists.
func toolNames(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

// TestLoopCallsAnMCPTool is exit criterion 1 of M6: a run completes a goal
// that requires an out-of-process peer, through the allowlist, the ledger and
// the envelope, with nothing special-cased for MCP anywhere in the loop.
//
// It exists because the adapter's own tests stop at the tools.Tool boundary.
// Everything this asserts is a property of the seam between that boundary and
// the loop, which is exactly where an adapter that satisfies its unit tests
// can still be wired in wrong.
func TestLoopCallsAnMCPTool(t *testing.T) {
	mcpTools := mcpFixture(t, testserver.Options{})

	f := newFixture(t, fake.New(
		fake.ToolUse("m1", "legal__search_dockets", map[string]any{"query": "arbitration"}, usage),
		fake.ToolUse("m2", "legal__fetch_docket", map[string]any{"docket_id": "24-1041"}, usage),
		finishCall("m3", "The exemption turns on the work performed (24-1041)."),
	))
	for _, tool := range mcpTools {
		require.NoError(t, f.registry.Register(tool))
	}

	id := f.submit("find the arbitration matter and read it", runOpts{
		tools: append(toolNames(mcpTools), builtin.FinishName),
	})
	stop := f.startWorker("mcp-worker", 0)
	defer stop()

	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	// The ledger holds exactly one row per call: an external tool is dispatched
	// through the same idempotency path as every other one.
	var requested, succeeded int
	var results []string
	for _, e := range f.events(id) {
		switch e.Type {
		case runtime.EventToolRequested:
			requested++
		case runtime.EventToolSucceeded:
			succeeded++
			var p struct {
				Result json.RawMessage `json:"result"`
			}
			require.NoError(t, json.Unmarshal(e.Payload, &p))
			results = append(results, string(p.Result))
		case runtime.EventToolFailed:
			t.Fatalf("unexpected tool_failed: %s", e.Payload)
		}
	}
	require.Equal(t, 3, requested)
	require.Equal(t, 3, succeeded)

	// The server's answer actually came back, rather than an empty result that
	// a status-only assertion would have called a pass.
	require.Contains(t, results[0], "Hendricks v. Bayard Logistics")
	require.Contains(t, results[1], "transportation worker")

	// And it reached the model as data, inside the envelope the system prompt
	// declares is never an instruction.
	// The body of a tool_result block is ContentBlock.Content, already
	// enveloped by the loop — not .Text, which is only set on text blocks.
	state := f.state(id)
	var enveloped bool
	for _, m := range state.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Content, "<tool_result") && strings.Contains(b.Content, "Hendricks") {
				enveloped = true
			}
		}
	}
	require.True(t, enveloped, "MCP result should reach the model inside the tool_result envelope")
}

// TestMCPToolOutsideAllowlistIsRefused pins the property §10 actually cares
// about: a capability the process holds is not a capability the run holds. The
// allowlist is fixed at submission, so a model that reaches for an MCP tool the
// run was not granted gets a tool failure rather than the call.
func TestMCPToolOutsideAllowlistIsRefused(t *testing.T) {
	mcpTools := mcpFixture(t, testserver.Options{})

	f := newFixture(t, fake.New(
		fake.ToolUse("m1", "legal__fetch_docket", map[string]any{"docket_id": "24-1041"}, usage),
		finishCall("m2", "could not reach it"),
	))
	for _, tool := range mcpTools {
		require.NoError(t, f.registry.Register(tool))
	}

	// Granted search, not fetch, though the worker can run both.
	id := f.submit("read docket 24-1041", runOpts{
		tools: []string{"legal__search_dockets", builtin.FinishName},
	})
	stop := f.startWorker("mcp-allowlist", 0)
	defer stop()

	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	var failed bool
	for _, e := range f.events(id) {
		if e.Type == runtime.EventToolFailed {
			failed = true
			require.Contains(t, string(e.Payload), "legal__fetch_docket")
		}
	}
	require.True(t, failed, "a tool outside the run's allowlist should fail rather than run")
}

// TestPoisonedMCPDescriptionReachesTheModel is the uncomfortable half of the
// injection story, asserted rather than asserted away.
//
// ADR-30 scores exposure before resistance, and this is the exposure: a
// server's tool description lands in the model's tool definitions, which are
// outside every envelope, so the poisoned text provably reaches the model. No
// filter in the adapter changes that — the description has to reach the model
// for the tool to be usable at all. What bounds the attack is the trust
// boundary (servers are operator config, not retrieved content) and the
// allowlist, which the test above pins.
//
// A test that asserted the instruction was stripped would be asserting a
// defence this package deliberately does not implement (ADR-35).
func TestPoisonedMCPDescriptionReachesTheModel(t *testing.T) {
	clean := mcpFixture(t, testserver.Options{})
	poisoned := mcpFixture(t, testserver.Options{PoisonedDescription: true})

	describe := func(ts []tools.Tool, name string) string {
		for _, tool := range ts {
			if tool.Name() == name {
				return tool.Description()
			}
		}
		t.Fatalf("no tool %q", name)
		return ""
	}

	before := describe(clean, "legal__search_dockets")
	after := describe(poisoned, "legal__search_dockets")
	require.NotEqual(t, before, after, "the poisoned fixture should change the description")

	// Exposure: the instruction survives into what the model is told the tool
	// is. This is the finding, not a failure.
	require.Contains(t, strings.ToLower(after), "24-1041",
		"the poisoned description reaches the model verbatim — the envelope does not cover tool definitions (ADR-35)")

	// Blast radius: what the adapter does bound is size, so a server cannot
	// flood the context window through a description it is never called on.
	oversized := mcpFixture(t, testserver.Options{OversizedDescription: true})
	got := describe(oversized, "legal__search_dockets")
	require.LessOrEqual(t, len(got), mcp.MaxDescriptionBytes,
		"an oversized description must be capped, not passed through")
	require.Less(t, len(got), testserver.OversizedDescriptionBytes)
}
