package local

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
)

// TestLiveToolCall talks to a real local runtime. It only runs when
// AGENTD_LIVE_MODEL=1, because it needs Ollama (or equivalent) on the box
// with the model already pulled.
func TestLiveToolCall(t *testing.T) {
	if os.Getenv("AGENTD_LIVE_MODEL") == "" {
		t.Skip("set AGENTD_LIVE_MODEL=1 to run against a local model")
	}
	baseURL := os.Getenv("AGENTD_MODEL_URL")
	if baseURL == "" {
		baseURL = "http://localhost:11434/v1"
	}
	name := os.Getenv("AGENTD_MODEL")
	if name == "" {
		name = "qwen2.5:7b"
	}

	p := New(Config{BaseURL: baseURL, Timeout: 5 * time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	resp, err := p.Complete(ctx, model.Request{
		Model:  name,
		System: "You must use the compute_deadline tool to answer. Do not compute dates yourself.",
		Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentBlock{
			{Type: model.BlockText, Text: "What date is 30 days after 2026-09-03? Use the tool."},
		}}},
		Tools: []model.ToolDef{{
			Name:        "compute_deadline",
			Description: "Add days to a YYYY-MM-DD date.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"start_date":{"type":"string"},"days":{"type":"integer"}},"required":["start_date","days"]}`),
		}},
	})
	require.NoError(t, err)
	t.Logf("model=%s stop=%s usage=%+v content=%+v", resp.Model, resp.StopReason, resp.Usage, resp.Content)
	require.Equal(t, model.StopToolUse, resp.StopReason)
	uses := resp.ToolUses()
	require.Len(t, uses, 1)
	require.Equal(t, "compute_deadline", uses[0].Name)
	require.Greater(t, resp.Usage.InputTokens, int64(0))
}
