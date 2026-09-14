package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
)

// serve stands in for the Messages API. It captures the decoded request body
// and answers with status/body. The SDK is pointed at it with BaseURL, so the
// whole client path is exercised, not just the translation helpers.
func serve(t *testing.T, status int, body string, cfg Config) (*Provider, *map[string]any, *atomic.Int32) {
	t.Helper()
	got := map[string]any{}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/v1/messages", r.URL.Path)
		require.Equal(t, "test-key", r.Header.Get("x-api-key"))
		require.NotEmpty(t, r.Header.Get("anthropic-version"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL
	cfg.MaxRetries = -1
	return New(cfg), &got, &calls
}

const toolUseResponse = `{
  "id": "msg_01", "type": "message", "role": "assistant", "model": "claude-opus-5",
  "content": [
    {"type": "thinking", "thinking": "", "signature": "sig-abc"},
    {"type": "text", "text": "Let me compute that."},
    {"type": "tool_use", "id": "toolu_01", "name": "compute_deadline", "input": {"start_date": "2026-09-03", "days": 30}},
    {"type": "tool_use", "id": "toolu_02", "name": "finish", "input": {}}
  ],
  "stop_reason": "tool_use", "stop_sequence": null,
  "usage": {"input_tokens": 1200, "output_tokens": 80, "cache_read_input_tokens": 1000, "cache_creation_input_tokens": 200}
}`

func TestCompleteTranslatesBothWays(t *testing.T) {
	p, got, _ := serve(t, 200, toolUseResponse, Config{ThinkingDisplay: "summarized"})

	temp := 0.2
	req := model.Request{
		Model:  "claude-opus-5",
		System: "sys",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: []model.ContentBlock{{Type: model.BlockText, Text: "goal"}}},
			{Role: model.RoleAssistant, Content: []model.ContentBlock{
				{Type: model.BlockThinking, Thinking: "", Signature: "sig-prev"},
				{Type: model.BlockRedactedThinking, Data: "opaque"},
				{Type: model.BlockText, Text: "working"},
				{Type: model.BlockToolUse, ToolUseID: "c0", Name: "x", Input: json.RawMessage(`{"a":1}`)},
			}},
			{Role: model.RoleUser, Content: []model.ContentBlock{
				{Type: model.BlockToolResult, ToolUseID: "c0", Content: "<tool_result>…</tool_result>", IsError: true},
			}},
		},
		Tools: []model.ToolDef{{
			Name:        "x",
			Description: "d",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"}},"required":["a"],"additionalProperties":false}`),
		}},
		MaxTokens:   256,
		Temperature: &temp,
	}
	resp, err := p.Complete(context.Background(), req)
	require.NoError(t, err)

	// Outbound: the wire body is Anthropic-shaped with nothing lost.
	body := *got
	require.Equal(t, "claude-opus-5", body["model"])
	require.EqualValues(t, 256, body["max_tokens"])
	require.Equal(t, 0.2, body["temperature"])
	require.Equal(t, map[string]any{"type": "adaptive", "display": "summarized"}, body["thinking"])
	require.Equal(t, []any{map[string]any{"type": "text", "text": "sys"}}, body["system"])

	msgs := body["messages"].([]any)
	require.Len(t, msgs, 3)
	require.Equal(t, "user", msgs[0].(map[string]any)["role"])
	assistant := msgs[1].(map[string]any)
	require.Equal(t, "assistant", assistant["role"])
	require.Equal(t, []any{
		map[string]any{"type": "thinking", "thinking": "", "signature": "sig-prev"},
		map[string]any{"type": "redacted_thinking", "data": "opaque"},
		map[string]any{"type": "text", "text": "working"},
		map[string]any{"type": "tool_use", "id": "c0", "name": "x", "input": map[string]any{"a": float64(1)}},
	}, assistant["content"], "thinking blocks are echoed back untouched, in order")
	result := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	require.Equal(t, "tool_result", result["type"])
	require.Equal(t, "c0", result["tool_use_id"])
	require.Equal(t, true, result["is_error"])
	require.Equal(t, []any{map[string]any{"type": "text", "text": "<tool_result>…</tool_result>"}}, result["content"])

	tools := body["tools"].([]any)
	require.Len(t, tools, 1)
	tool := tools[0].(map[string]any)
	require.Equal(t, "x", tool["name"])
	require.Equal(t, "d", tool["description"])
	require.Equal(t, map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"a": map[string]any{"type": "integer"}},
		"required":             []any{"a"},
		"additionalProperties": false,
	}, tool["input_schema"], "the registry's JSON Schema goes through verbatim, extra keywords included")

	// Inbound.
	require.Equal(t, "claude-opus-5", resp.Model)
	require.Equal(t, model.StopToolUse, resp.StopReason)
	require.Equal(t, model.Usage{InputTokens: 1200, OutputTokens: 80, CacheReadInputTokens: 1000, CacheCreationInputTokens: 200}, resp.Usage)
	require.Len(t, resp.Content, 4)
	require.Equal(t, model.ContentBlock{Type: model.BlockThinking, Signature: "sig-abc"}, resp.Content[0])
	require.Equal(t, "Let me compute that.", resp.Text())
	uses := resp.ToolUses()
	require.Len(t, uses, 2)
	require.Equal(t, "toolu_01", uses[0].ToolUseID)
	require.Equal(t, "compute_deadline", uses[0].Name)
	require.JSONEq(t, `{"start_date":"2026-09-03","days":30}`, string(uses[0].Input))
	require.JSONEq(t, `{}`, string(uses[1].Input))

	// Real money: 1200 in @ $5 + 80 out @ $25 + 1000 cache read @ $0.50 + 200 cache write @ $6.25.
	require.Equal(t, int64(6000+2000+500+1250), p.CostMicroUSD(resp.Model, resp.Usage))
}

func TestCompleteDefaultsAndStopReasons(t *testing.T) {
	end := `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	p, got, _ := serve(t, 200, end, Config{})
	resp, err := p.Complete(context.Background(), model.Request{Model: "claude-opus-5", Messages: []model.Message{
		{Role: model.RoleUser, Content: []model.ContentBlock{{Type: model.BlockText, Text: "hi"}}},
	}})
	require.NoError(t, err)
	require.Equal(t, model.StopEndTurn, resp.StopReason)
	require.Equal(t, "hi", resp.Text())
	body := *got
	require.EqualValues(t, DefaultMaxTokens, body["max_tokens"], "max_tokens defaults when the run sets none")
	_, hasThinking := body["thinking"]
	require.False(t, hasThinking, "no thinking parameter unless configured")
	_, hasTemp := body["temperature"]
	require.False(t, hasTemp)
	_, hasSystem := body["system"]
	require.False(t, hasSystem)

	for wire, want := range map[string]string{
		"max_tokens":                    model.StopMaxTokens,
		"model_context_window_exceeded": model.StopMaxTokens,
		"stop_sequence":                 model.StopEndTurn,
		"something_new":                 "something_new",
	} {
		p, _, _ := serve(t, 200, `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":"`+wire+`","usage":{"input_tokens":1,"output_tokens":1}}`, Config{})
		resp, err := p.Complete(context.Background(), model.Request{Model: "claude-opus-5"})
		require.NoError(t, err, wire)
		require.Equal(t, want, resp.StopReason, wire)
	}
}

func TestCompleteRefusalIsAnError(t *testing.T) {
	p, _, _ := serve(t, 200, `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5","content":[],
		"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"nope"},
		"usage":{"input_tokens":1,"output_tokens":0}}`, Config{})
	_, err := p.Complete(context.Background(), model.Request{Model: "claude-opus-5"})
	var refusal *RefusalError
	require.ErrorAs(t, err, &refusal, "the typed error survives the non-retryable marker")
	require.Equal(t, "cyber", refusal.Category)
	require.Equal(t, "nope", refusal.Explanation)
	require.ErrorContains(t, err, "refused")
	require.True(t, model.IsNonRetryable(err), "the model declined the request, not this attempt at it")
}

func TestCompleteErrors(t *testing.T) {
	p, _, calls := serve(t, 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, Config{})
	_, err := p.Complete(context.Background(), model.Request{Model: "claude-opus-5"})
	require.ErrorContains(t, err, "HTTP 401")
	require.ErrorContains(t, err, "authentication_error")
	require.ErrorContains(t, err, "invalid x-api-key")
	require.Equal(t, int32(1), calls.Load(), "4xx is not retried")
	require.True(t, model.IsNonRetryable(err), "a bad credential never starts working")

	p, _, calls = serve(t, 200, `{}`, Config{})
	_, err = p.Complete(context.Background(), model.Request{Model: "claude-unpriced-9"})
	require.ErrorContains(t, err, "no price configured")
	require.Equal(t, int32(0), calls.Load(), "refused before any request is made")
	require.True(t, model.IsNonRetryable(err), "a missing price is configuration, not weather")

	_, err = p.Complete(context.Background(), model.Request{})
	require.ErrorContains(t, err, "model is required")

	_, err = p.Complete(context.Background(), model.Request{Model: "claude-opus-5", Messages: []model.Message{
		{Role: model.RoleUser, Content: []model.ContentBlock{{Type: "image"}}},
	}})
	require.ErrorContains(t, err, `unsupported content block type "image"`)

	_, err = p.Complete(context.Background(), model.Request{Model: "claude-opus-5", Tools: []model.ToolDef{{Name: "t", InputSchema: json.RawMessage(`not json`)}}})
	require.ErrorContains(t, err, "input schema")

	p = New(Config{APIKey: "k", BaseURL: "http://127.0.0.1:1", MaxRetries: -1})
	_, err = p.Complete(context.Background(), model.Request{Model: "claude-opus-5"})
	require.Error(t, err)
	var apiErr *RefusalError
	require.False(t, errors.As(err, &apiErr))
	require.False(t, model.IsNonRetryable(err), "a transport failure has no status to judge, so it is retried")
}

// TestCompleteStatusRetryability pins the split ADR-27 draws: the statuses
// that describe the request are fatal, the ones that describe the moment are
// not. The error text is unchanged either way — only how the loop treats it.
func TestCompleteStatusRetryability(t *testing.T) {
	const body = `{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`
	for _, tc := range []struct {
		status       int
		nonRetryable bool
	}{
		{400, true},  // a malformed body stays malformed
		{401, true},  // a bad credential never starts working
		{403, true},  // a forbidden caller stays forbidden
		{404, true},  // an unknown model stays unknown
		{422, true},  // an unprocessable request stays unprocessable
		{408, false}, // a timeout deserves backoff
		{409, false},
		{429, false}, // a rate limit is exactly what a retry is for
		{500, false},
		{529, false}, // overloaded
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			p, _, calls := serve(t, tc.status, body, Config{})
			_, err := p.Complete(context.Background(), model.Request{Model: "claude-opus-5"})
			require.Error(t, err)
			require.Equal(t, tc.nonRetryable, model.IsNonRetryable(err))
			require.ErrorContains(t, err, fmt.Sprintf("HTTP %d", tc.status), "the marker does not rewrite the message")
			require.Equal(t, int32(1), calls.Load(), "SDK retries are off in this fixture")
		})
	}
}

func TestMaxOutputTokens(t *testing.T) {
	p := New(Config{APIKey: "k"})
	require.Equal(t, DefaultMaxTokens, p.MaxOutputTokens("claude-opus-5"))
	require.Equal(t, DefaultMaxTokens, p.MaxOutputTokens("claude-haiku-4-5"),
		"the cap is the provider's substitute for an absent max_tokens, so it does not vary by model")
}

func TestPricingTable(t *testing.T) {
	p := New(Config{APIKey: "k", Price: map[string]model.Price{"claude-custom": {InputPerMTok: 1_000_000}}})
	price, ok := p.Price("claude-opus-5")
	require.True(t, ok)
	require.Equal(t, int64(5_000_000), price.InputPerMTok)
	require.Equal(t, int64(25_000_000), price.OutputPerMTok)
	require.Equal(t, int64(500_000), price.CacheReadPerMTok)
	require.Equal(t, int64(6_250_000), price.CacheWritePerMTok)

	price, ok = p.Price("claude-opus-4-5-20251101")
	require.True(t, ok, "dated snapshots match by prefix")
	require.Equal(t, int64(5_000_000), price.InputPerMTok)

	price, ok = p.Price("claude-fable-5-1")
	require.True(t, ok)
	require.Equal(t, int64(250_000), price.CacheReadPerMTok, "longest prefix wins over claude-fable-5")

	price, ok = p.Price("claude-custom-2")
	require.True(t, ok)
	require.Equal(t, int64(1_000_000), price.InputPerMTok)

	_, ok = p.Price("gpt-4")
	require.False(t, ok)
	require.Equal(t, int64(0), p.CostMicroUSD("gpt-4", model.Usage{InputTokens: 1_000_000}))
}

func TestParsePrices(t *testing.T) {
	got, err := ParsePrices(" claude-opus-6=6/30, claude-haiku-5 = 1.5 / 7.5 ,")
	require.NoError(t, err)
	require.Equal(t, map[string]model.Price{
		"claude-opus-6":  {InputPerMTok: 6_000_000, OutputPerMTok: 30_000_000, CacheReadPerMTok: 600_000, CacheWritePerMTok: 7_500_000},
		"claude-haiku-5": {InputPerMTok: 1_500_000, OutputPerMTok: 7_500_000, CacheReadPerMTok: 150_000, CacheWritePerMTok: 1_875_000},
	}, got)

	empty, err := ParsePrices("")
	require.NoError(t, err)
	require.Empty(t, empty)

	for _, bad := range []string{"claude", "claude=5", "claude=x/5", "claude=5/y", "claude=-1/5"} {
		_, err := ParsePrices(bad)
		require.Error(t, err, bad)
	}
}

// TestCompleteMalformedRequestsAreNonRetryable covers the failures that happen
// before the request is ever sent. They are the clearest case ADR-27 exists
// for: nothing about the network decided them, so the two backoffs the loop
// would otherwise spend buy a strictly identical answer.
func TestCompleteMalformedRequestsAreNonRetryable(t *testing.T) {
	p, _, calls := serve(t, 200, `{}`, Config{})

	t.Run("no model", func(t *testing.T) {
		_, err := p.Complete(context.Background(), model.Request{})
		require.ErrorContains(t, err, "model is required")
		require.True(t, model.IsNonRetryable(err))
	})

	t.Run("unknown role", func(t *testing.T) {
		_, err := p.Complete(context.Background(), model.Request{
			Model:    "claude-opus-5",
			Messages: []model.Message{{Role: "system", Content: []model.ContentBlock{{Type: model.BlockText, Text: "hi"}}}},
		})
		require.ErrorContains(t, err, `unknown role "system"`)
		require.True(t, model.IsNonRetryable(err))
	})

	t.Run("bad tool schema", func(t *testing.T) {
		_, err := p.Complete(context.Background(), model.Request{
			Model: "claude-opus-5",
			Tools: []model.ToolDef{{Name: "broken", InputSchema: json.RawMessage(`"not an object"`)}},
		})
		require.ErrorContains(t, err, "input schema")
		require.True(t, model.IsNonRetryable(err))
	})

	require.Zero(t, calls.Load(), "none of these reached the API at all")
}
