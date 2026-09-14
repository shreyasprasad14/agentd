package cassette_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/cassette"
)

// envelope mirrors runtime.Envelope. The cassette cannot import the runtime —
// a model provider must not drag in the store and the telemetry stack — so the
// format is duplicated here and the real coupling is asserted end to end by
// TestCassetteMatchesRuntimeEnvelope in internal/evals.
func envelope(tool string, seq int, body string) string {
	return fmt.Sprintf("<tool_result tool=%q seq=%d>\n%s\n</tool_result>", tool, seq, body)
}

func request(messages ...model.Message) model.Request {
	return model.Request{
		Model:    "qwen2.5:7b",
		System:   "you are a careful assistant",
		Messages: messages,
		Tools:    []model.ToolDef{{Name: "search_corpus"}, {Name: "finish"}},
	}
}

func toolResult(seq int, body string) model.Message {
	return model.Message{Role: model.RoleUser, Content: []model.ContentBlock{{
		Type:      model.BlockToolResult,
		ToolUseID: "tu_1",
		Content:   envelope("search_corpus", seq, body),
	}}}
}

func TestKeyIgnoresVolatileFields(t *testing.T) {
	// The same search, run twice: chunk ids are fresh uuids on every ingest
	// and the duration is a wall clock.
	a := request(toolResult(5, `{"hits":[{"chunk_id":"aaaa-1","score":0.51,"content":"x"}],"duration_ms":12}`))
	b := request(toolResult(5, `{"hits":[{"chunk_id":"bbbb-2","score":0.52,"content":"x"}],"duration_ms":907}`))

	require.NotEqual(t, model.HashRequest(a), model.HashRequest(b),
		"the exact fingerprint should see these as different; that is what makes it the event log's value")
	require.Equal(t, cassette.Key(a, cassette.DefaultVolatile()), cassette.Key(b, cassette.DefaultVolatile()),
		"the cassette key must not")
}

// TestKeyIgnoresEnvelopeSeq is the regression test for the crash case. A run
// that dies inside a model call comes back with a second model_requested in
// the log, so every later event sits one seq higher — and the seq is printed
// into the envelope of every subsequent tool result.
func TestKeyIgnoresEnvelopeSeq(t *testing.T) {
	clean := request(toolResult(5, `{"ok":true}`))
	resumed := request(toolResult(6, `{"ok":true}`))

	require.NotEqual(t, model.HashRequest(clean), model.HashRequest(resumed))
	require.Equal(t, cassette.Key(clean, cassette.DefaultVolatile()), cassette.Key(resumed, cassette.DefaultVolatile()))
}

func TestKeySeesRealChanges(t *testing.T) {
	base := request(toolResult(5, `{"hits":[{"chunk_id":"a","content":"Miranda"}]}`))
	vol := cassette.DefaultVolatile()

	t.Run("content", func(t *testing.T) {
		other := request(toolResult(5, `{"hits":[{"chunk_id":"a","content":"Carpenter"}]}`))
		require.NotEqual(t, cassette.Key(base, vol), cassette.Key(other, vol))
	})
	t.Run("system prompt", func(t *testing.T) {
		other := base
		other.System += " and terse"
		require.NotEqual(t, cassette.Key(base, vol), cassette.Key(other, vol))
	})
	t.Run("tool list", func(t *testing.T) {
		other := base
		other.Tools = []model.ToolDef{{Name: "search_corpus"}}
		require.NotEqual(t, cassette.Key(base, vol), cassette.Key(other, vol))
	})
	t.Run("model", func(t *testing.T) {
		other := base
		other.Model = "claude-opus-5"
		require.NotEqual(t, cassette.Key(base, vol), cassette.Key(other, vol))
	})
	t.Run("an extra turn", func(t *testing.T) {
		other := request(toolResult(5, `{"hits":[{"chunk_id":"a","content":"Miranda"}]}`),
			model.Message{Role: model.RoleAssistant, Content: []model.ContentBlock{{Type: model.BlockText, Text: "ok"}}})
		require.NotEqual(t, cassette.Key(base, vol), cassette.Key(other, vol))
	})
}

func TestKeyDottedPathScopesTheStrip(t *testing.T) {
	// "document.id" must reach the document's id and leave a hit's id alone.
	with := func(docID, hitID string) model.Request {
		return request(toolResult(1, fmt.Sprintf(`{"document":{"id":%q,"source_id":"clop-0002"},"hits":[{"id":%q}]}`, docID, hitID)))
	}
	vol := []string{"document.id"}
	require.Equal(t, cassette.Key(with("a", "same"), vol), cassette.Key(with("b", "same"), vol))
	require.NotEqual(t, cassette.Key(with("a", "one"), vol), cassette.Key(with("a", "two"), vol))
}

func TestNormalizeLeavesNonEnvelopesAlone(t *testing.T) {
	// tool_failed results are plain text, not JSON, and a tool is free to
	// return something that is neither.
	plain := model.Message{Role: model.RoleUser, Content: []model.ContentBlock{{
		Type: model.BlockToolResult, Content: envelope("run_python", 4, "error: run cancelled"), IsError: true,
	}}}
	out := cassette.Normalize(request(plain), cassette.DefaultVolatile())
	require.Contains(t, out.Messages[0].Content[0].Content, "error: run cancelled")
	require.NotContains(t, out.Messages[0].Content[0].Content, "seq=")
}

func TestNormalizeDoesNotMutateItsInput(t *testing.T) {
	req := request(toolResult(5, `{"duration_ms":12,"ok":true}`))
	before, err := json.Marshal(req)
	require.NoError(t, err)

	_ = cassette.Key(req, cassette.DefaultVolatile())

	after, err := json.Marshal(req)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after),
		"the loop sends this same Request to the provider; normalising must not edit it")
}

func TestNormalizeCanonicalisesKeyOrder(t *testing.T) {
	a := request(toolResult(1, `{"b":2,"a":1}`))
	b := request(toolResult(1, `{"a":1,"b":2}`))
	require.Equal(t, cassette.Key(a, cassette.DefaultVolatile()), cassette.Key(b, cassette.DefaultVolatile()))
}
