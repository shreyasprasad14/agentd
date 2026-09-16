package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/tools"
	"github.com/shreyasprasad/agentd/internal/tools/mcp/testserver"
)

// invoke calls a discovered tool the way the loop would.
func invoke(t *testing.T, tool tools.Tool, args string) (tools.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return tool.Invoke(ctx, tools.Invocation{RunID: uuid.New(), Seq: 1, Args: json.RawMessage(args)})
}

func decode(t *testing.T, res tools.Result) Output {
	t.Helper()
	var out Output
	require.NoError(t, json.Unmarshal(res.Content, &out))
	return out
}

func TestDiscoveryNamespacesToolsAndKeepsTheServersSchema(t *testing.T) {
	c, _ := dialTest(t, stdioConfig(t, testserver.Options{}))

	require.ElementsMatch(t,
		[]string{"legal__search_dockets", "legal__fetch_docket", "legal__check_citation"},
		toolNames(c))

	search := toolNamed(t, c, "legal__search_dockets")
	require.Equal(t, "legal", search.Server())
	require.Equal(t, "search_dockets", search.RemoteName())
	require.Equal(t, tools.External, search.TrustTier())
	require.Contains(t, search.Description(), "Search a docket index")

	// The server's own schema, not a substitute: the model has to see the
	// arguments the server actually accepts.
	var schema struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(search.Schema(), &schema))
	require.Equal(t, "object", schema.Type)
	require.Contains(t, schema.Properties, "query")
	require.Contains(t, schema.Properties, "court")
	require.Equal(t, "words to match against docket captions and summaries", schema.Properties["query"]["description"])
}

// The namespacing exists so a server cannot collide with a builtin, and a
// collision would not be a bad tool call — it would be a binary that refuses
// to start, because Register rejects duplicates at boot.
func TestNamespacedToolsRegisterAlongsideBuiltins(t *testing.T) {
	c, _ := dialTest(t, stdioConfig(t, testserver.Options{}))

	reg := tools.NewRegistry()
	require.NoError(t, reg.Register(fakeBuiltin{name: "finish"}))
	require.NoError(t, reg.Register(fakeBuiltin{name: "search_dockets"}))
	for _, tool := range c.Tools() {
		require.NoError(t, reg.Register(tool), "registering %s", tool.Name())
	}

	require.Contains(t, reg.Names(), "legal__search_dockets")
	require.Contains(t, reg.Names(), "search_dockets")

	// An MCP tool is subject to the allowlist like anything else.
	_, err := reg.Resolve("legal__search_dockets", []string{"finish"}, json.RawMessage(`{"query":"x"}`))
	require.ErrorIs(t, err, tools.ErrNotAllowed)

	got, err := reg.Resolve("legal__search_dockets", []string{"legal__search_dockets"}, json.RawMessage(`{"query":"x"}`))
	require.NoError(t, err)
	require.Equal(t, "legal__search_dockets", got.Name())

	// And to its schema: the registry validated the server's own document.
	_, err = reg.Resolve("legal__search_dockets", []string{"legal__search_dockets"}, json.RawMessage(`{"query":42}`))
	require.Error(t, err)
	var verr *tools.ValidationError
	require.ErrorAs(t, err, &verr)
}

func TestInvokeReturnsFlattenedContent(t *testing.T) {
	c, _ := dialTest(t, stdioConfig(t, testserver.Options{}))

	res, err := invoke(t, toolNamed(t, c, "legal__search_dockets"), `{"query":"aquifer"}`)
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
	require.True(t, res.Cost.IsZero(), "an external tool spends no model budget of ours")

	out := decode(t, res)
	require.False(t, out.IsError)
	require.False(t, out.Truncated)
	require.Contains(t, out.Content, "23-8872")
	require.Contains(t, out.Content, "Coastal Aquifer")
}

// IsError is the tool failing, which the model must be able to see and
// correct from; it is not the protocol failing, so Invoke returns no error.
func TestInvokeMapsIsErrorToAModelVisibleResult(t *testing.T) {
	c, _ := dialTest(t, stdioConfig(t, testserver.Options{}))

	res, err := invoke(t, toolNamed(t, c, "legal__fetch_docket"), `{"docket_id":"not-a-docket"}`)
	require.NoError(t, err, "a tool that says no is a result, not an error")
	require.Equal(t, 1, res.ExitCode)

	out := decode(t, res)
	require.True(t, out.IsError)
	require.Contains(t, out.Content, "no docket with id")

	// The session is untouched, so the next call works.
	res, err = invoke(t, toolNamed(t, c, "legal__fetch_docket"), `{"docket_id":"24-1041"}`)
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
	require.Contains(t, decode(t, res).Content, "Hendricks")
}

func TestServerThatDiesMidCallFailsThatCallAndReconnects(t *testing.T) {
	c, logs := dialTest(t, stdioConfig(t, testserver.Options{Crashable: true}))
	// Shrunk so the test observes a reconnect without sleeping for a quarter
	// of a second first.
	c.base, c.max = time.Millisecond, 10*time.Millisecond

	first := c.PID()
	require.NotZero(t, first)

	_, err := invoke(t, toolNamed(t, c, "legal__crash_server"), `{}`)
	require.Error(t, err, "a dead server is the protocol failing, not the tool")
	require.Contains(t, err.Error(), "legal__crash_server")
	require.Zero(t, c.PID(), "the dead session is torn down, not held open")
	require.Contains(t, logs.String(), "mcp session lost")

	// Lazily, on the next call, a fresh subprocess answers.
	require.Eventually(t, func() bool {
		res, err := invoke(t, toolNamed(t, c, "legal__search_dockets"), `{"query":"aquifer"}`)
		return err == nil && res.ExitCode == 0
	}, 10*time.Second, 20*time.Millisecond)

	require.NotZero(t, c.PID())
	require.NotEqual(t, first, c.PID(), "reconnect means a new process, not the corpse")
	require.Contains(t, logs.String(), "mcp reconnected")
}

func TestReconnectBacksOffRatherThanRespawningOnEveryCall(t *testing.T) {
	c, _ := dialTest(t, stdioConfig(t, testserver.Options{Crashable: true}))
	c.base, c.max = time.Hour, time.Hour

	_, err := invoke(t, toolNamed(t, c, "legal__crash_server"), `{}`)
	require.Error(t, err)

	_, err = invoke(t, toolNamed(t, c, "legal__search_dockets"), `{"query":"aquifer"}`)
	require.ErrorIs(t, err, ErrUnavailable)
	require.Zero(t, c.PID(), "no subprocess is spawned while the backoff is running")
}

// A call that hangs must be cut off by the per-call timeout, and a timeout is
// not evidence the session died — the server is still there, it was just slow.
func TestCallTimeoutDoesNotTearDownAHealthySession(t *testing.T) {
	cfg := stdioConfig(t, testserver.Options{Stallable: true})
	cfg.Timeout = Duration(200 * time.Millisecond)
	c, _ := dialTest(t, cfg)
	pid := c.PID()

	started := time.Now()
	_, err := invoke(t, toolNamed(t, c, "legal__stall"), `{}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out after 200ms")
	require.Less(t, time.Since(started), 10*time.Second)

	require.Equal(t, pid, c.PID(), "a slow call is not a dead server")
	res, err := invoke(t, toolNamed(t, c, "legal__search_dockets"), `{"query":"aquifer"}`)
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
}

// Exit criterion 2: a worker shutdown leaves no orphaned stdio subprocess.
func TestCloseReapsTheStdioSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal 0 liveness probing is a POSIX idiom")
	}
	log, _ := testLogger()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, stdioConfig(t, testserver.Options{}), log)
	require.NoError(t, err)

	pid := c.PID()
	require.NotZero(t, pid)
	require.True(t, alive(pid), "the fixture server should be running before Close")

	require.NoError(t, c.Close())
	require.False(t, alive(pid), "Close must reap the child, not just drop the reference")
	require.Zero(t, c.PID())

	// Close is idempotent, because a worker shutting down on an error path
	// may well reach it twice. A tool held by something that outlived the
	// shutdown fails cleanly rather than respawning the server behind it.
	require.NoError(t, c.Close())
	_, err = invoke(t, toolNamed(t, c, "legal__search_dockets"), `{"query":"x"}`)
	require.ErrorIs(t, err, ErrClosed)
}

// alive reports whether a process we reaped is really gone. Signal 0 checks
// for existence without delivering anything.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func TestConnectSkipsUnreachableServers(t *testing.T) {
	log, logs := testLogger()
	good := stdioConfig(t, testserver.Options{})
	missing := ServerConfig{Name: "ghost", Transport: TransportStdio, Command: "/nonexistent/mcp-server-binary"}
	dead := ServerConfig{Name: "offline", Transport: TransportHTTP, URL: "http://127.0.0.1:1/mcp"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, closer, err := Connect(ctx, Config{Servers: []ServerConfig{missing, good, dead}}, log)
	require.NoError(t, err, "an unreachable server is a warning, not a boot failure")
	require.NotNil(t, closer)
	t.Cleanup(func() { require.NoError(t, closer.Close()) })

	var names []string
	for _, tool := range list {
		names = append(names, tool.Name())
	}
	require.ElementsMatch(t,
		[]string{"legal__search_dockets", "legal__fetch_docket", "legal__check_citation"}, names)

	require.Contains(t, logs.String(), "mcp server unavailable")
	require.Contains(t, logs.String(), "ghost")
	require.Contains(t, logs.String(), "offline")

	// The registry therefore never hears of the missing servers' tools, which
	// is what makes a run naming one fail with "unknown tool" rather than
	// being silently granted a capability it was not allowlisted for.
	reg := tools.NewRegistry()
	for _, tool := range list {
		require.NoError(t, reg.Register(tool))
	}
	_, err = reg.Resolve("ghost__anything", []string{"ghost__anything"}, nil)
	require.ErrorIs(t, err, tools.ErrUnknownTool)
}

func TestConnectRejectsABadConfigRatherThanStarting(t *testing.T) {
	log, _ := testLogger()
	_, _, err := Connect(context.Background(), Config{Servers: []ServerConfig{
		{Name: "a", Transport: TransportStdio, Command: "x"},
		{Name: "a", Transport: TransportStdio, Command: "y"},
	}}, log)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate server name")
}

func TestConnectWithNoServers(t *testing.T) {
	log, _ := testLogger()
	list, closer, err := Connect(context.Background(), Config{}, log)
	require.NoError(t, err)
	require.Empty(t, list)
	require.NotNil(t, closer)
	require.NoError(t, closer.Close())
}

func TestHTTPTransport(t *testing.T) {
	srv := httptest.NewServer(testserver.Handler(testserver.Options{}))
	t.Cleanup(srv.Close)

	c, _ := dialTest(t, ServerConfig{Name: "legal", Transport: TransportHTTP, URL: srv.URL})
	require.Contains(t, toolNames(c), "legal__check_citation")
	require.Zero(t, c.PID(), "nothing was spawned, so there is nothing to reap")

	res, err := invoke(t, toolNamed(t, c, "legal__check_citation"), `{"citation":"410 U.S. 113"}`)
	require.NoError(t, err)
	require.Contains(t, decode(t, res).Content, `"valid":true`)
}

// TestHTTPTransportListsAnOversizedManifest is a regression test for a cap in
// the wrong slot: the streamable transport's MaxEventSize was MaxResultBytes,
// which bounds one *result*, while the limit applies to every message — and
// the largest legal message is the manifest. A server advertising a
// description past the cap was therefore not truncated over HTTP, it was
// unlistable, and the operator saw "server unavailable" naming nothing.
//
// The stdio sibling of this case (TestServerWithHostileManifestStillRegisters)
// passed throughout, which is the point: a bound that lives in the transport
// makes the same hostile server behave differently depending on how it was
// configured, so the stdio test could not see this.
func TestHTTPTransportListsAnOversizedManifest(t *testing.T) {
	srv := httptest.NewServer(testserver.Handler(testserver.Options{OversizedDescription: true}))
	t.Cleanup(srv.Close)

	c, _ := dialTest(t, ServerConfig{Name: "legal", Transport: TransportHTTP, URL: srv.URL})
	require.Contains(t, toolNames(c), "legal__search_dockets")

	// Truncated by this package on arrival, which is where the bound belongs:
	// the same cap, and the same truncation marker, as over stdio.
	desc := toolNamed(t, c, "legal__search_dockets").Description()
	require.LessOrEqual(t, len(desc), MaxDescriptionBytes)
	require.True(t, strings.HasSuffix(desc, truncationMarker))

	// A manifest this package would accept must fit the transport, or the cap
	// is refusing schemas its own MaxSchemaBytes allows.
	require.Greater(t, MaxEventBytes, MaxSchemaBytes)
	require.Greater(t, MaxEventBytes, MaxResultBytes)
}

// fakeBuiltin stands in for a real builtin in registry tests, so this package
// does not depend on the builtin package to prove a name does not collide.
type fakeBuiltin struct{ name string }

func (f fakeBuiltin) Name() string               { return f.name }
func (f fakeBuiltin) Description() string        { return "stub " + f.name }
func (f fakeBuiltin) Schema() json.RawMessage    { return json.RawMessage(`{"type":"object"}`) }
func (f fakeBuiltin) TrustTier() tools.TrustTier { return tools.Builtin }
func (f fakeBuiltin) Invoke(context.Context, tools.Invocation) (tools.Result, error) {
	return tools.Result{Content: json.RawMessage(`{}`)}, nil
}
