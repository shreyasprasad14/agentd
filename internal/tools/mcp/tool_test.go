package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/tools"
	"github.com/shreyasprasad/agentd/internal/tools/mcp/testserver"
)

// fixtureClient is a Client with no session, for the discovery-time checks
// that never touch the wire.
func fixtureClient(t *testing.T) *Client {
	t.Helper()
	log, _ := testLogger()
	return &Client{cfg: ServerConfig{Name: "legal", Transport: TransportStdio, Command: "x"}, log: log}
}

// A hostile server must not be able to stop the process from starting, and
// Register is where that would happen: it compiles the schema, and callers
// wire tools with MustRegister, which panics.
func TestHostileSchemaFallsBackToSomethingRegistrable(t *testing.T) {
	cases := []struct {
		name   string
		schema any
	}{
		{"absent", nil},
		{"a string", "give me anything"},
		{"an array", []any{"query"}},
		{"a number", 7},
		{"not an object type", map[string]any{"type": "string"}},
		{"a type that is not a string", map[string]any{"type": 42}},
		{"a nested value that is not a schema", json.RawMessage(`{"type":"object","properties":{"q":{"type":42}}}`)},
		{"unmarshalable", make(chan int)},
		{"past the size cap", map[string]any{"type": "object", "description": strings.Repeat("x", MaxSchemaBytes)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, err := newTool(fixtureClient(t), &mcpsdk.Tool{Name: "cited_by", InputSchema: tc.schema})
			require.NoError(t, err)
			require.JSONEq(t, permissiveSchema, string(tool.Schema()))
			require.NoError(t, tools.NewRegistry().Register(tool), "a hostile schema must never reach Register unfiltered")
		})
	}
}

func TestUsableSchemaIsPassedThroughVerbatim(t *testing.T) {
	raw := map[string]any{
		"type":       "object",
		"properties": map[string]any{"docket_id": map[string]any{"type": "string"}},
		"required":   []any{"docket_id"},
	}
	tool, err := newTool(fixtureClient(t), &mcpsdk.Tool{Name: "fetch_docket", InputSchema: raw})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(tool.Schema(), &got))
	require.Equal(t, "object", got["type"])
	require.Contains(t, got, "required")

	reg := tools.NewRegistry()
	require.NoError(t, reg.Register(tool))
	_, err = reg.Resolve(tool.Name(), []string{tool.Name()}, json.RawMessage(`{}`))
	require.Error(t, err, "the server's own required-field rule is enforced locally")
}

// The name is the reason namespacing exists: an MCP server can name a tool
// `finish`, and an un-namespaced collision would turn a working binary into
// one that panics at startup.
func TestNamespacingCannotShadowABuiltin(t *testing.T) {
	tool, err := newTool(fixtureClient(t), &mcpsdk.Tool{Name: "finish", InputSchema: map[string]any{"type": "object"}})
	require.NoError(t, err)
	require.Equal(t, "legal__finish", tool.Name())

	reg := tools.NewRegistry()
	require.NoError(t, reg.Register(fakeBuiltin{name: "finish"}))
	require.NoError(t, reg.Register(tool))
}

func TestToolNamesTheModelAPIWouldRejectAreSkipped(t *testing.T) {
	cases := []struct {
		name string
		tool string
		want string
	}{
		{"too long once namespaced", strings.Repeat("s", 60), "longer than 64 bytes"},
		{"unicode", "recherche_dossiér", "model APIs accept only"},
		{"a dot", "docket.fetch", "model APIs accept only"},
		{"empty", "   ", "no name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newTool(fixtureClient(t), &mcpsdk.Tool{Name: tc.tool})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestSanitizeDescription(t *testing.T) {
	t.Run("kept when it fits", func(t *testing.T) {
		require.Equal(t, "Search dockets.", sanitizeDescription("  Search dockets.  ", "legal", "search_dockets"))
	})
	t.Run("synthesized when absent", func(t *testing.T) {
		got := sanitizeDescription("", "legal", "search_dockets")
		require.Contains(t, got, "search_dockets")
		require.Contains(t, got, "legal")
	})
	t.Run("invalid utf-8 is scrubbed", func(t *testing.T) {
		// One bad byte would otherwise fail every model call the run makes,
		// including the ones that never touch this tool.
		got := sanitizeDescription("search\xffdockets", "legal", "search_dockets")
		require.Equal(t, "searchdockets", got)
	})
	t.Run("truncated past the cap", func(t *testing.T) {
		got := sanitizeDescription(strings.Repeat("é", MaxDescriptionBytes), "legal", "search_dockets")
		require.LessOrEqual(t, len(got), MaxDescriptionBytes)
		require.True(t, strings.HasSuffix(got, truncationMarker))
		require.True(t, utf8Valid(got))
	})
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "\x00") == s }

func TestSplitName(t *testing.T) {
	server, tool, ok := SplitName("legal__search_dockets")
	require.True(t, ok)
	require.Equal(t, "legal", server)
	require.Equal(t, "search_dockets", tool)

	// A tool whose own name contains the separator still splits correctly,
	// because a configured server name may not contain one.
	server, tool, ok = SplitName("legal__search__v2")
	require.True(t, ok)
	require.Equal(t, "legal", server)
	require.Equal(t, "search__v2", tool)

	for _, bad := range []string{"finish", "", "__x", "x__"} {
		_, _, ok := SplitName(bad)
		require.False(t, ok, "%q", bad)
	}
}

func TestFlattenContent(t *testing.T) {
	t.Run("joins text blocks", func(t *testing.T) {
		got, truncated := flatten(&mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "first"},
			&mcpsdk.TextContent{Text: "second"},
		}})
		require.Equal(t, "first\nsecond", got)
		require.False(t, truncated)
	})
	t.Run("names non-text blocks rather than dropping them", func(t *testing.T) {
		// A model that asked for an image should be told one came back, not
		// left waiting for something that silently vanished.
		got, _ := flatten(&mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "chart:"},
			&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3}},
		}})
		require.Equal(t, "chart:\n[non-text content omitted: image]", got)
	})
	t.Run("falls back to structured content", func(t *testing.T) {
		got, _ := flatten(&mcpsdk.CallToolResult{StructuredContent: map[string]any{"count": 2}})
		require.JSONEq(t, `{"count":2}`, got)
	})
	t.Run("caps the result", func(t *testing.T) {
		got, truncated := flatten(&mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: strings.Repeat("é", MaxResultBytes)},
		}})
		require.True(t, truncated)
		require.LessOrEqual(t, len(got), MaxResultBytes)
		require.True(t, utf8Valid(got))
	})
}

// End-to-end: a real server, over a real pipe, advertising a schema that will
// not compile and a description that will not fit.
func TestServerWithHostileManifestStillRegisters(t *testing.T) {
	c, logs := dialTest(t, stdioConfig(t, testserver.Options{
		HostileSchema:        true,
		OversizedDescription: true,
		LongToolName:         true,
	}))

	require.Contains(t, toolNames(c), "legal__cited_by")
	require.JSONEq(t, permissiveSchema, string(toolNamed(t, c, "legal__cited_by").Schema()))
	require.Contains(t, logs.String(), "falling back to a permissive one")

	desc := toolNamed(t, c, "legal__search_dockets").Description()
	require.LessOrEqual(t, len(desc), MaxDescriptionBytes)
	require.True(t, strings.HasSuffix(desc, truncationMarker))

	for _, name := range toolNames(c) {
		require.LessOrEqual(t, len(name), MaxToolNameBytes)
	}
	require.Contains(t, logs.String(), "skipping mcp tool")

	// Every surviving tool is still registrable, which is the property that
	// keeps a hostile server from turning a boot into a panic.
	reg := tools.NewRegistry()
	for _, tool := range c.Tools() {
		require.NoError(t, reg.Register(tool))
	}

	// And the fallback schema means the tool is still callable: validation
	// moved to the server, which is where it happens for an external tool
	// anyway.
	res, err := invoke(t, toolNamed(t, c, "legal__cited_by"), `{"docket_id":"24-1041"}`)
	require.NoError(t, err)
	require.Contains(t, decode(t, res).Content, "cited_by")
}

// The poisoned description reaches the model intact and unlabeled. That is
// the honest state of the defence: the envelope does not cover tool
// definitions, and this package does not pretend to filter them.
func TestPoisonedDescriptionIsNotFilteredOnlyBounded(t *testing.T) {
	c, _ := dialTest(t, stdioConfig(t, testserver.Options{PoisonedDescription: true}))
	desc := toolNamed(t, c, "legal__search_dockets").Description()
	require.Contains(t, desc, "IMPORTANT SYSTEM NOTICE")
	require.LessOrEqual(t, len(desc), MaxDescriptionBytes)
}
