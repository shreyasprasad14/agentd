// Package mcp adapts Model Context Protocol servers into tools.Tool (spec
// §8). A Client is one connected server; a Tool is one capability that server
// advertises, registered under the namespaced name "<server>__<tool>" and
// running at the tools.External trust tier. Servers are declared in a config
// file read at boot, and both `agentd serve` and `agentd work` read the same
// one, because the API fills a run's default allowlist from its registry and
// the worker dispatches through its own (ADR-34, docs/plans/m6.md).
//
// # Tool descriptions are an injection surface the envelope does not cover
//
// Spec §10's threat model is about retrieved content: a court opinion whose
// text reads as an instruction. The loop's <tool_result> envelope answers
// that, and ADR-33 hardened it against content that forges its own
// delimiters. An MCP server introduces a second surface, shaped differently,
// which the envelope does not and cannot defend against.
//
// A tool's name, description, and JSON Schema are written by the server
// operator and go into the model's *tool definitions* — which is to say into
// the part of the request that is outside every envelope, because tool
// definitions are not tool results. A hostile or compromised server can put
// an instruction in a description and every model call for the rest of the
// run carries it. This is the tool-poisoning attack, and the honest position
// is that the envelope was never meant to stop it. No amount of parsing this
// package could do would change that: the text has to reach the model for the
// tool to be usable at all.
//
// What bounds the attack is the trust boundary, not a filter. An MCP server
// is operator configuration read from a file at boot, like a binary on the
// PATH — adding one is a trust decision equivalent to installing software,
// not equivalent to retrieving a document. A run cannot name its own server,
// which would invert that boundary by letting a submission add a capability
// to the process. And because a run's allowlist is fixed at submission and
// validated against the registry there, a poisoned description can only ask
// the model to use capabilities the run was already granted; no tool with
// real side effects is reachable unless the run was configured for it.
//
// On top of that boundary this package adds the cheap mechanical defences,
// which are about blast radius rather than trust: MaxDescriptionBytes,
// MaxSchemaBytes, MaxResultBytes and MaxTools bound how much a server can
// push into one request, a discovered tool whose name cannot survive
// namespacing is dropped rather than registered, and the manifest is pinned
// at boot so a server that reconnects cannot change what the registry holds
// under a run that was already admitted against it. None of these make a
// hostile server safe. They stop a hostile or merely broken one from filling
// the context window or from turning a working binary into one that refuses
// to start.
//
// Deliberately not done: prefixing every description with a "this text came
// from server X" banner. It reads like a defence, it costs tokens in every
// model call, and nothing measures whether it helps. M6 adds an INJECTION
// eval case with a poisoned description instead, so the claim made here has a
// number behind it rather than a mitigation that was never scored (ADR-30
// scores exposure first, then resistance).
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/shreyasprasad/agentd/internal/tools"
)

// Separator joins a server name to a tool name. Two underscores rather than a
// dot or a slash because the hosted model APIs constrain tool names to
// [a-zA-Z0-9_-]; a separator the API rejects would fail every model call in
// the run rather than just the offending tool.
const Separator = "__"

const (
	// MaxToolNameBytes is the longest namespaced name that may be
	// registered. The bound is the model API's, not ours: Anthropic and the
	// OpenAI-compatible local runtimes both cap tool names at 64 bytes, and a
	// name over the cap poisons the whole request, not one tool call.
	MaxToolNameBytes = 64

	// MaxDescriptionBytes bounds a discovered tool's description. It is
	// truncated rather than rejected: a description is advisory, so half of
	// one still leaves a callable tool, while dropping the tool would let a
	// server disable its own capability by being verbose.
	MaxDescriptionBytes = 4 << 10

	// MaxSchemaBytes bounds the advertised input schema. Past the cap the
	// schema is replaced with a permissive one rather than truncated, because
	// half a JSON document is not a JSON document.
	MaxSchemaBytes = 32 << 10

	// MaxResultBytes bounds one tool result, matching the corpus tools' cap
	// for the same reason: retrieved text must not crowd the context window
	// the way an uncapped stdout could.
	MaxResultBytes = 24 << 10

	// MaxTools bounds how many tools one server may contribute. A server that
	// advertises thousands would otherwise push every builtin out of the
	// model's attention, and `GET /v1/tools` with it.
	MaxTools = 128

	// MaxEventBytes bounds one message off an HTTP server's SSE stream.
	//
	// It is deliberately not MaxResultBytes, which is what it used to be and
	// which was wrong in a way only a manifest shows: the transport cap
	// applies to *every* message, and the largest legal one is not a result
	// but the tools/list response carrying a whole manifest. With the result
	// cap in that slot, a server whose schemas this package would happily
	// accept (MaxSchemaBytes alone is larger than MaxResultBytes) could not
	// be listed at all, and the operator saw "server unavailable" rather than
	// anything naming the real cause.
	//
	// Nothing is lost by widening it. A result is truncated to
	// MaxResultBytes on arrival regardless of how it got here, so this is a
	// backstop against a stream that never terminates, not the bound on what
	// reaches the model. The stdio transport has no equivalent limit, which
	// is the other half of the argument: a cap that changes a hostile
	// server's blast radius depending on which transport it was configured
	// with is not a security boundary, it is an inconsistency.
	MaxEventBytes = MaxTools * (MaxDescriptionBytes + MaxSchemaBytes)
)

// DefaultCallTimeout bounds one tools/call when the server's config does not
// say. It matches the sandbox's default wall clock for the same reason: a
// tool that hangs must not hold a cancel open longer than a tool that runs
// (ADR-25).
const DefaultCallTimeout = 30 * time.Second

// DefaultConnectTimeout bounds the boot-time connect and tool discovery for
// one server, so a server that accepts a connection and then says nothing
// delays the process by a bounded amount rather than forever. Ten seconds
// matches the Docker ping in buildSandbox.
const DefaultConnectTimeout = 10 * time.Second

// ErrClosed is returned by a Client whose Close has already run.
var ErrClosed = errors.New("mcp client is closed")

// ErrUnavailable is returned when a server is disconnected and its backoff
// has not expired. It reaches the model as a tool failure it can route
// around, which is the point: the run keeps going without the capability
// rather than failing on it.
var ErrUnavailable = errors.New("mcp server is unavailable")

// Connect dials every server in cfg and returns the tools they advertise,
// together with a Closer that shuts all of them down.
//
// A server that cannot be reached logs a warning and contributes no tools
// rather than failing the process, which is how cmd/agentd already treats the
// sandbox: runs that never touch it must keep working. The consequence is
// stated rather than papered over — a run submitted while a server was up and
// dispatched while it was down fails that tool call with "unknown tool",
// which the loop reports to the model as a tool failure and which the model
// can route around. A run is never silently granted a capability it was not
// allowlisted for, which is the property §10 actually cares about.
//
// The returned error is reserved for a config that is wrong, which is an
// operator mistake worth refusing to start on; an unreachable server is not
// one.
func Connect(ctx context.Context, cfg Config, log *slog.Logger) ([]tools.Tool, io.Closer, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, fmt.Errorf("mcp config: %w", err)
	}
	set := &Set{}
	var out []tools.Tool
	for _, sc := range cfg.Servers {
		dialCtx, cancel := context.WithTimeout(ctx, DefaultConnectTimeout)
		c, err := Dial(dialCtx, sc, log)
		cancel()
		if err != nil {
			log.Warn("mcp server unavailable; its tools will not be registered",
				"server", sc.Name, "transport", string(sc.Transport), "error", err)
			continue
		}
		set.clients = append(set.clients, c)
		out = append(out, c.Tools()...)
		log.Info("mcp server ready", "server", sc.Name, "transport", string(sc.Transport),
			"tools", len(c.Tools()), "pid", c.PID())
	}
	return out, set, nil
}

// Set is the group of servers Connect opened. Closing it closes all of them,
// which for a stdio server means reaping its subprocess: a worker shutdown
// that left the children running would leak one process per server per
// restart.
type Set struct {
	clients []*Client
}

// Clients returns the servers that connected, for a caller that wants to
// report on them. The slice is not copied; do not modify it.
func (s *Set) Clients() []*Client { return s.clients }

// Close shuts every server down, returning the joined errors so one server
// that refuses to die does not hide the others.
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, c := range s.clients {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("mcp %s: %w", c.Name(), err))
		}
	}
	return errors.Join(errs...)
}
