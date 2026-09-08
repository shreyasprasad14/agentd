package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
)

// TestLiveToolCall makes one real Messages API call. It only runs when
// AGENTD_LIVE_ANTHROPIC=1, because it spends money and needs credentials
// (ANTHROPIC_API_KEY, or a profile from `ant auth login`).
func TestLiveToolCall(t *testing.T) {
	if os.Getenv("AGENTD_LIVE_ANTHROPIC") == "" {
		t.Skip("set AGENTD_LIVE_ANTHROPIC=1 to call the real API")
	}
	name := os.Getenv("AGENTD_ANTHROPIC_MODEL")
	if name == "" {
		name = DefaultModel
	}

	p := New(Config{Timeout: 5 * time.Minute, ThinkingDisplay: os.Getenv("AGENTD_ANTHROPIC_THINKING_DISPLAY")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tool := model.ToolDef{
		Name:        "compute_deadline",
		Description: "Add days to a YYYY-MM-DD date.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"start_date":{"type":"string"},"days":{"type":"integer"}},"required":["start_date","days"],"additionalProperties":false}`),
	}
	messages := []model.Message{{Role: model.RoleUser, Content: []model.ContentBlock{
		{Type: model.BlockText, Text: "What date is 30 days after 2026-09-03? Use the compute_deadline tool; do not compute it yourself."},
	}}}

	resp, err := p.Complete(ctx, model.Request{
		Model:    name,
		System:   "You must use the compute_deadline tool to answer. Do not compute dates yourself.",
		Messages: messages,
		Tools:    []model.ToolDef{tool},
	})
	require.NoError(t, err)
	t.Logf("model=%s stop=%s usage=%+v cost=%s content=%+v", resp.Model, resp.StopReason, resp.Usage,
		model.FormatUSD(p.CostMicroUSD(resp.Model, resp.Usage)), resp.Content)
	require.Equal(t, model.StopToolUse, resp.StopReason)
	uses := resp.ToolUses()
	require.Len(t, uses, 1)
	require.Equal(t, "compute_deadline", uses[0].Name)
	require.Greater(t, resp.Usage.InputTokens, int64(0))
	require.Greater(t, p.CostMicroUSD(resp.Model, resp.Usage), int64(0), "a hosted call is never free")

	// Second turn: echo the assistant turn (thinking blocks included) with a
	// tool result, exactly as the loop rebuilds it from the event log. This
	// is what fails if thinking blocks are not round-tripped correctly.
	messages = append(messages,
		model.Message{Role: model.RoleAssistant, Content: resp.Content},
		model.Message{Role: model.RoleUser, Content: []model.ContentBlock{{
			Type: model.BlockToolResult, ToolUseID: uses[0].ToolUseID,
			Content: `<tool_result tool="compute_deadline" seq=4>` + "\n" + `{"deadline":"2026-10-03"}` + "\n</tool_result>",
		}}},
	)
	resp2, err := p.Complete(ctx, model.Request{
		Model:    name,
		System:   "You must use the compute_deadline tool to answer. Do not compute dates yourself.",
		Messages: messages,
		Tools:    []model.ToolDef{tool},
	})
	require.NoError(t, err)
	t.Logf("turn 2: stop=%s usage=%+v text=%q", resp2.StopReason, resp2.Usage, resp2.Text())
	require.Contains(t, resp2.Text(), "2026-10-03")
}
