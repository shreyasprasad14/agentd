package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/shreyasprasad/agentd/internal/tools"
)

// permissiveSchema is what a tool gets when its server's schema cannot be
// trusted to reach tools.Registry. It accepts any object, which moves
// argument validation to the server — where, for an external tool, it happens
// anyway.
const permissiveSchema = `{"type":"object"}`

// truncationMarker is appended to a description the cap cut short, so the
// model is told the text ends early rather than reading a sentence that stops
// mid-word as if it were complete.
const truncationMarker = "\n\n[truncated by agentd]"

// Tool is one capability discovered on one MCP server, adapted to
// tools.Tool. It holds no session of its own: the Client owns that, so a
// reconnect is invisible to every Tool the server contributed.
type Tool struct {
	client *Client
	// server and remote are the two halves of name, kept apart because
	// remote is what goes on the wire and name is what the registry, the
	// allowlist, and the event log use.
	server  string
	remote  string
	name    string
	desc    string
	schema  json.RawMessage
	timeout time.Duration
}

// Output is what the model sees, inside the loop's <tool_result> envelope.
// It is a struct rather than the raw MCP content array because the model
// reads text, and because IsError has to survive as a distinct field: it is
// the difference between "the tool says no" and "the tool is broken", and the
// two want different next moves.
type Output struct {
	// Content is the server's text blocks, joined.
	Content string `json:"content"`
	// IsError marks a failure the tool reported about itself — bad
	// arguments, no such record — which the model can read and correct.
	IsError bool `json:"is_error,omitempty"`
	// Truncated says MaxResultBytes cut the content short.
	Truncated bool `json:"truncated,omitempty"`
}

// newTool adapts one entry of a server's manifest. It returns an error for a
// tool that cannot be registered at all, which the caller logs and skips.
func newTool(c *Client, t *mcpsdk.Tool) (*Tool, error) {
	if t == nil || strings.TrimSpace(t.Name) == "" {
		return nil, errors.New("tool has no name")
	}
	name := c.cfg.Name + Separator + t.Name
	if err := validToolName(name); err != nil {
		// Dropped rather than renamed: a generated substitute name would not
		// match what an operator wrote in a run's allowlist, so the tool
		// would be registered and permanently un-grantable, which is worse
		// than absent.
		return nil, fmt.Errorf("namespaced name %q is unusable: %w", name, err)
	}
	schema, ok := sanitizeSchema(t.InputSchema, name)
	if !ok {
		c.log.Warn("mcp tool advertises a schema agentd will not compile; falling back to a permissive one",
			"tool", name)
	}
	return &Tool{
		client:  c,
		server:  c.cfg.Name,
		remote:  t.Name,
		name:    name,
		desc:    sanitizeDescription(t.Description, c.cfg.Name, t.Name),
		schema:  schema,
		timeout: c.cfg.CallTimeout(),
	}, nil
}

// Name is the namespaced name: "<server>__<tool>". A server can therefore
// never shadow or collide with a builtin like `finish`, which matters more
// than it sounds — Registry.Register rejects duplicates at boot, so an
// un-namespaced collision would turn a working binary into one that refuses
// to start because of a name chosen on the other side of a socket (ADR-34).
func (t *Tool) Name() string { return t.name }

// Server is the configured server name this tool came from.
func (t *Tool) Server() string { return t.server }

// RemoteName is the name the tool has on its server, which is what goes on
// the wire and what appears in that server's own logs.
func (t *Tool) RemoteName() string { return t.remote }

// Description is the server's description, capped and made valid UTF-8. It is
// attacker-influenced text that lands outside every envelope; see the package
// doc for why that is a trust boundary rather than a parsing problem.
func (t *Tool) Description() string { return t.desc }

// Schema is the server's input schema, or a permissive object schema when
// what the server sent could not be trusted to compile.
func (t *Tool) Schema() json.RawMessage { return t.schema }

// TrustTier is External: the code ran in a process this runtime does not own.
func (t *Tool) TrustTier() tools.TrustTier { return tools.External }

// Invoke performs one tools/call.
//
// The two failure modes MCP distinguishes are kept distinct, because they
// call for different handling. A result with IsError set is the *tool*
// failing — bad arguments, a record that does not exist — and it comes back
// as a successful Result the model can read inside the envelope and correct
// itself from. A Go error is the *protocol* failing — the server died, the
// socket closed, the call timed out — and it comes back as an error, which
// the loop records as tool_failed. Collapsing the first into the second would
// hide a self-correctable mistake behind a failure event; collapsing the
// second into the first would tell the model a dead server said something.
func (t *Tool) Invoke(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	// Derived, not replacing: the loop reads ctx.Err() on the parent to tell
	// a cancelled run from a failed tool call, so a timeout that fires here
	// must leave the parent's error nil (ADR-25).
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	res, err := t.client.call(callCtx, t.remote, inv.Args)
	if err != nil {
		if ctx.Err() == nil && callCtx.Err() != nil {
			return tools.Result{}, fmt.Errorf("mcp %s: call timed out after %s: %w", t.name, t.timeout, err)
		}
		return tools.Result{}, fmt.Errorf("mcp %s: %w", t.name, err)
	}

	out := Output{IsError: res.IsError}
	out.Content, out.Truncated = flatten(res)
	content, err := json.Marshal(out)
	if err != nil {
		return tools.Result{}, fmt.Errorf("mcp %s: encode result: %w", t.name, err)
	}
	// ExitCode mirrors the python tool's convention: nonzero means the call
	// did not do what it was asked, and the event log can be filtered on it
	// without parsing the result body. Cost stays zero — an external tool may
	// well spend money, but not *this* runtime's model budget, and reporting
	// spend agentd cannot account for would make the ledger disagree with the
	// invoice (ADR-23).
	exit := 0
	if res.IsError {
		exit = 1
	}
	return tools.Result{Content: content, ExitCode: exit}, nil
}

// SplitName splits a registered name back into its server and tool halves. It
// splits on the first separator, which is unambiguous because a configured
// server name may not contain one (see ServerConfig.Validate).
func SplitName(name string) (server, tool string, ok bool) {
	s, t, found := strings.Cut(name, Separator)
	if !found || s == "" || t == "" {
		return "", "", false
	}
	return s, t, true
}

// validToolName enforces what the model APIs accept. A name they reject does
// not fail one tool call; it fails every model call in every run the tool is
// allowlisted for, which is why this is checked at discovery and not at use.
func validToolName(name string) error {
	if len(name) > MaxToolNameBytes {
		return fmt.Errorf("longer than %d bytes", MaxToolNameBytes)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return fmt.Errorf("contains %q; model APIs accept only [a-zA-Z0-9_-]", r)
		}
	}
	return nil
}

// sanitizeDescription bounds what one server can push into every model call
// for the rest of a run.
//
// Invalid UTF-8 is scrubbed first, and that is not cosmetic: the description
// is marshaled into the model request body, so one bad byte from one server
// would fail every call the run makes, including the ones that never touch
// that tool. The cap then truncates on a rune boundary, and a tool that
// describes itself as nothing at all gets a synthesized line, because a model
// given a nameless capability tends to either ignore it or guess at it.
func sanitizeDescription(desc, server, tool string) string {
	desc = strings.ToValidUTF8(strings.TrimSpace(desc), "")
	if desc == "" {
		return fmt.Sprintf("Tool %q on the MCP server %q. The server supplied no description.", tool, server)
	}
	if len(desc) <= MaxDescriptionBytes {
		return desc
	}
	cut := MaxDescriptionBytes - len(truncationMarker)
	for cut > 0 && !utf8.RuneStart(desc[cut]) {
		cut--
	}
	return desc[:cut] + truncationMarker
}

// sanitizeSchema turns a server's advertised input schema into something
// tools.Registry is guaranteed to accept, reporting whether the server's own
// schema survived.
//
// The guarantee is the point. Register compiles the schema and returns an
// error if it will not compile, and callers wire builtins with MustRegister,
// which panics — so a server that advertises `"inputSchema": 7` would be able
// to stop the binary from starting. Every way that can happen ends in the
// same permissive object schema: absent, unmarshalable, not a JSON object,
// over MaxSchemaBytes (half a JSON document is not a JSON document, so this
// one cannot be truncated), declaring a non-object type (which would reject
// every call the model could make), or failing to compile under the same
// compiler the registry uses. The cost of falling back is that arguments go
// unvalidated locally and the server rejects them itself; the cost of not
// falling back is a runtime that a peer can crash at boot.
func sanitizeSchema(raw any, name string) (json.RawMessage, bool) {
	if raw == nil {
		return json.RawMessage(permissiveSchema), false
	}
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > MaxSchemaBytes {
		return json.RawMessage(permissiveSchema), false
	}
	var obj map[string]any
	if err := json.Unmarshal(encoded, &obj); err != nil {
		// Not a JSON object: a string, a number, an array, or null.
		return json.RawMessage(permissiveSchema), false
	}
	if typ, ok := obj["type"]; ok {
		if s, isString := typ.(string); !isString || s != "object" {
			return json.RawMessage(permissiveSchema), false
		}
	}
	if !compiles(encoded, name) {
		return json.RawMessage(permissiveSchema), false
	}
	return json.RawMessage(encoded), true
}

// compiles reports whether tools.Registry will be able to compile this
// schema. It duplicates the registry's compile step on purpose: asking the
// question here costs one compile at boot, and not asking it means the answer
// arrives as a panic in MustRegister.
func compiles(schema json.RawMessage, name string) bool {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return false
	}
	c := jsonschema.NewCompiler()
	url := "agentd://tools/" + name + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return false
	}
	_, err = c.Compile(url)
	return err == nil
}

// flatten joins the result's text blocks into the one string the model reads,
// and reports whether MaxResultBytes cut it short.
//
// Non-text blocks are named rather than dropped silently, so a model that
// asked for an image is told an image came back and can stop waiting for it.
// Structured content is used only when there is no text at all: the SDK
// populates both for a typed server handler, and emitting both would put the
// same payload in the context window twice.
func flatten(res *mcpsdk.CallToolResult) (string, bool) {
	var b strings.Builder
	for _, block := range res.Content {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if text, ok := block.(*mcpsdk.TextContent); ok {
			b.WriteString(text.Text)
			continue
		}
		b.WriteString("[non-text content omitted: " + contentKind(block) + "]")
	}
	if b.Len() == 0 && res.StructuredContent != nil {
		if encoded, err := json.Marshal(res.StructuredContent); err == nil {
			b.Write(encoded)
		}
	}
	return capText(strings.ToValidUTF8(b.String(), ""))
}

// contentKind reads a block's wire "type" rather than switching on the SDK's
// concrete types, so a content kind added to the protocol after this was
// written is named correctly instead of falling into a default case.
func contentKind(block mcpsdk.Content) string {
	encoded, err := block.MarshalJSON()
	if err != nil {
		return "unknown"
	}
	var shape struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(encoded, &shape); err != nil || shape.Type == "" {
		return "unknown"
	}
	return shape.Type
}

// capText truncates on a rune boundary and says whether it had to.
func capText(s string) (string, bool) {
	if len(s) <= MaxResultBytes {
		return s, false
	}
	cut := MaxResultBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
