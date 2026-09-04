package local

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
)

// serve returns a fake chat-completions endpoint that captures the request
// body and replies with body.
func serve(t *testing.T, status int, body string) (*Provider, *wireRequest) {
	t.Helper()
	var got wireRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "Bearer k", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL + "/v1/", APIKey: "k"}), &got
}

func TestCompleteTranslatesToolCalls(t *testing.T) {
	p, got := serve(t, 200, `{
	  "model": "qwen2.5:7b",
	  "choices": [{"message": {"role": "assistant", "content": "Let me compute that.",
	    "tool_calls": [
	      {"id": "call_1", "type": "function", "function": {"name": "compute_deadline", "arguments": "{\"start_date\":\"2026-09-03\",\"days\":30}"}},
	      {"id": "", "type": "function", "function": {"name": "finish", "arguments": "not json"}}
	    ]}, "finish_reason": "tool_calls"}],
	  "usage": {"prompt_tokens": 120, "completion_tokens": 30}
	}`)

	temp := 0.2
	req := model.Request{
		Model:  "qwen2.5:7b",
		System: "sys",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: []model.ContentBlock{{Type: model.BlockText, Text: "goal"}}},
			{Role: model.RoleAssistant, Content: []model.ContentBlock{
				{Type: model.BlockText, Text: "thinking"},
				{Type: model.BlockToolUse, ToolUseID: "c0", Name: "x", Input: json.RawMessage(`{"a":1}`)},
			}},
			{Role: model.RoleUser, Content: []model.ContentBlock{
				{Type: model.BlockToolResult, ToolUseID: "c0", Content: "<tool_result>…</tool_result>"},
			}},
		},
		Tools:       []model.ToolDef{{Name: "x", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens:   256,
		Temperature: &temp,
	}
	resp, err := p.Complete(context.Background(), req)
	require.NoError(t, err)

	// Outbound translation.
	require.Equal(t, "qwen2.5:7b", got.Model)
	require.False(t, got.Stream)
	require.Equal(t, 256, got.MaxTokens)
	require.Equal(t, 0.2, *got.Temperature)
	require.Len(t, got.Messages, 4)
	require.Equal(t, "system", got.Messages[0].Role)
	require.Equal(t, "sys", *got.Messages[0].Content)
	require.Equal(t, "user", got.Messages[1].Role)
	require.Equal(t, "goal", *got.Messages[1].Content)
	require.Equal(t, "assistant", got.Messages[2].Role)
	require.Equal(t, "thinking", *got.Messages[2].Content)
	require.Len(t, got.Messages[2].ToolCalls, 1)
	require.Equal(t, "c0", got.Messages[2].ToolCalls[0].ID)
	require.Equal(t, "x", got.Messages[2].ToolCalls[0].Function.Name)
	require.JSONEq(t, `{"a":1}`, got.Messages[2].ToolCalls[0].Function.Arguments)
	require.Equal(t, "tool", got.Messages[3].Role)
	require.Equal(t, "c0", got.Messages[3].ToolCallID)
	require.Len(t, got.Tools, 1)
	require.Equal(t, "function", got.Tools[0].Type)
	require.Equal(t, "x", got.Tools[0].Function.Name)

	// Inbound translation.
	require.Equal(t, "qwen2.5:7b", resp.Model)
	require.Equal(t, model.StopToolUse, resp.StopReason)
	require.Equal(t, model.Usage{InputTokens: 120, OutputTokens: 30}, resp.Usage)
	require.Len(t, resp.Content, 3)
	require.Equal(t, "Let me compute that.", resp.Content[0].Text)
	uses := resp.ToolUses()
	require.Len(t, uses, 2)
	require.Equal(t, "call_1", uses[0].ToolUseID)
	require.Equal(t, "compute_deadline", uses[0].Name)
	require.JSONEq(t, `{"start_date":"2026-09-03","days":30}`, string(uses[0].Input))
	require.NotEmpty(t, uses[1].ToolUseID, "missing ids are synthesised")
	require.JSONEq(t, `"not json"`, string(uses[1].Input), "garbage arguments stay valid JSON")
	require.Equal(t, int64(0), p.CostMicroUSD("qwen2.5:7b", resp.Usage))
}

func TestCompleteTextAndFinishReasons(t *testing.T) {
	p, _ := serve(t, 200, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	resp, err := p.Complete(context.Background(), model.Request{Model: "m"})
	require.NoError(t, err)
	require.Equal(t, model.StopEndTurn, resp.StopReason)
	require.Equal(t, "m", resp.Model, "falls back to the requested model")
	require.Equal(t, "hi", resp.Text())

	p, _ = serve(t, 200, `{"choices":[{"message":{"role":"assistant","content":"tru"},"finish_reason":"length"}]}`)
	resp, err = p.Complete(context.Background(), model.Request{Model: "m"})
	require.NoError(t, err)
	require.Equal(t, model.StopMaxTokens, resp.StopReason)
}

func TestCompleteErrors(t *testing.T) {
	p, _ := serve(t, 404, `{"error":{"message":"model 'nope' not found"}}`)
	_, err := p.Complete(context.Background(), model.Request{Model: "nope"})
	require.ErrorContains(t, err, "HTTP 404")
	require.ErrorContains(t, err, "not found")

	p, _ = serve(t, 200, `{"error":{"message":"overloaded"}}`)
	_, err = p.Complete(context.Background(), model.Request{Model: "m"})
	require.ErrorContains(t, err, "overloaded")

	p, _ = serve(t, 200, `{"choices":[]}`)
	_, err = p.Complete(context.Background(), model.Request{Model: "m"})
	require.ErrorContains(t, err, "no choices")

	p = New(Config{BaseURL: "http://127.0.0.1:1"})
	_, err = p.Complete(context.Background(), model.Request{Model: "m"})
	require.Error(t, err)
}

func TestPricing(t *testing.T) {
	p := New(Config{Price: map[string]model.Price{"paid": {InputPerMTok: 1_000_000, OutputPerMTok: 2_000_000}}})
	require.Equal(t, int64(3), p.CostMicroUSD("paid", model.Usage{InputTokens: 1, OutputTokens: 1}))
	require.Equal(t, int64(0), p.CostMicroUSD("free", model.Usage{InputTokens: 1_000_000}))
}
