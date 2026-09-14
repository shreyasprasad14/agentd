package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
)

// TestBudgetEstimateUsesTheLastMeasuredCount pins the shape of the estimate:
// one number the provider measured plus one number the loop counted. The rule
// of thumb is applied only to the second, which is what keeps an error here
// proportional to a single step's tool output rather than to the whole
// conversation.
func TestBudgetEstimateUsesTheLastMeasuredCount(t *testing.T) {
	state := &State{
		LastInputTokens: 1000,
		Messages: []model.Message{
			{Role: model.RoleUser, Content: []model.ContentBlock{
				{Type: model.BlockText, Text: "compute the deadline"},
			}},
			{Role: model.RoleAssistant, Content: []model.ContentBlock{
				{Type: model.BlockToolUse, ToolUseID: "t1", Name: "compute_deadline", Input: json.RawMessage(`{"days":30}`)},
			}},
			{Role: model.RoleUser, Content: []model.ContentBlock{
				{Type: model.BlockToolResult, ToolUseID: "t1", Content: strings.Repeat("x", 400)},
			}},
		},
	}
	req := model.Request{System: strings.Repeat("s", 4_000), Messages: state.Messages}

	// 400 characters of tool result is the only thing the next call carries
	// that the last one did not; everything before the assistant turn is
	// already inside the 1000 tokens the provider billed for.
	require.Equal(t, int64(1_100), nextInputTokens(state, req))

	// Which is to say the first term is carried, not recomputed: a system
	// prompt ten times the size does not move the answer, because the
	// provider already counted it once.
	req.System = strings.Repeat("s", 40_000)
	require.Equal(t, int64(1_100), nextInputTokens(state, req))
}

// TestBudgetEstimateFirstStepCountsTheWholeRequest covers the branch with
// nothing measured behind it. It is also the branch a provider that reports no
// usage at all falls into, where counting the entire conversation is the safe
// reading rather than a guess.
func TestBudgetEstimateFirstStepCountsTheWholeRequest(t *testing.T) {
	req := model.Request{
		System:   strings.Repeat("s", 100),
		Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentBlock{{Type: model.BlockText, Text: strings.Repeat("g", 40)}}}},
		Tools: []model.ToolDef{{
			Name:        "finish",
			Description: strings.Repeat("d", 50),
			InputSchema: json.RawMessage(strings.Repeat("c", 30)),
		}},
	}

	// 100 system + 40 goal + (6 + 50 + 30) tool definition = 226 characters.
	require.Equal(t, int64(56), nextInputTokens(&State{}, req))

	// Tool definitions are prompt the provider bills for even though nothing
	// in the conversation mentions them, so dropping them has to move the
	// number. A run with a large allowlist is exactly the run whose first call
	// is mispriced if they are forgotten.
	req.Tools = nil
	require.Equal(t, int64(35), nextInputTokens(&State{}, req))
}

// TestBudgetEstimateErrsHighNeverLow asserts a direction rather than a number,
// because the direction is the whole point. A ceiling that guesses low is a
// bill; a ceiling that guesses high is only a run that stopped with money
// left, which is the trade ADR-22 made deliberately. The estimate therefore
// prices the *whole* prompt at the uncached rate and assumes the model writes
// to its cap, neither of which a real call is obliged to do.
func TestBudgetEstimateErrsHighNeverLow(t *testing.T) {
	price := model.Price{
		InputPerMTok:     3_000_000,
		OutputPerMTok:    15_000_000,
		CacheReadPerMTok: 300_000, // a tenth of the input rate
	}
	p := fake.New().WithPrice(price).WithMaxOutputTokens(2_000)
	// A prompt the provider measured at 1000 tokens, and nothing appended
	// since, so the estimate is the measured figure alone.
	state := &State{LastInputTokens: 1_000}

	est := nextCallEstimate(p, state, model.Request{Model: "m"})
	require.Equal(t, int64(1_000), est.InputTokens)
	require.Equal(t, int64(2_000), est.MaxOutputTokens)
	require.Equal(t, price.Cost(model.Usage{InputTokens: 1_000, OutputTokens: 2_000}), est.MicroUSD)
	require.GreaterOrEqual(t, est.InputTokens, state.LastInputTokens,
		"the estimate may never fall below a prompt size the provider already billed for")

	// What that call actually costs when it comes back: the same 1000-token
	// prompt, but 900 of its tokens served from the prompt cache, and a model
	// that stopped well short of its cap.
	actual := price.Cost(model.Usage{InputTokens: 100, CacheReadInputTokens: 900, OutputTokens: 300})
	require.Greater(t, est.MicroUSD, actual,
		"an estimate below the real price is a bill; one above it is only a run that stopped early")
}

// TestBudgetEstimateOutputCapFallback covers the output term's resolution
// order. agent_config.max_tokens reaches the estimate as Request.MaxTokens —
// buildRequest is the only thing between them — and wins, because it is what
// the call will actually ask for. The provider's own cap is the fallback for
// the common case of a run that names no cap at all.
func TestBudgetEstimateOutputCapFallback(t *testing.T) {
	state := &State{LastInputTokens: 10}
	p := fake.New().WithPrice(model.Price{OutputPerMTok: 1_000_000}).WithMaxOutputTokens(16_000)

	est := nextCallEstimate(p, state, model.Request{Model: "m", MaxTokens: 512})
	require.Equal(t, int64(512), est.MaxOutputTokens, "the request's own cap wins over the provider's")
	require.Equal(t, int64(512), est.MicroUSD)
	require.Equal(t, "10 input + up to 512 output tokens", est.describe())

	est = nextCallEstimate(p, state, model.Request{Model: "m"})
	require.Equal(t, int64(16_000), est.MaxOutputTokens, "with nothing named, the provider's cap is what the call would get")
	require.Equal(t, int64(16_000), est.MicroUSD)

	// A provider that names no cap — the local one, where the server decides
	// and the spend is $0 either way — leaves the estimate standing on its
	// input term, and says so rather than quoting a silent zero.
	free := fake.New()
	est = nextCallEstimate(free, state, model.Request{Model: "m"})
	require.Zero(t, est.MaxOutputTokens)
	require.Equal(t, int64(10), est.InputTokens)
	require.Equal(t, "10 input tokens, no output cap", est.describe())

	// A negative cap is nonsense from a misconfigured provider, and it is
	// nonsense in the one direction that matters: left alone it would price
	// the call below its own input term, which is a ceiling rounding down.
	broken := fake.New().WithPrice(model.Price{InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000}).WithMaxOutputTokens(-1_000_000)
	est = nextCallEstimate(broken, state, model.Request{Model: "m"})
	require.Zero(t, est.MaxOutputTokens)
	require.Equal(t, int64(10), est.MicroUSD, "the input term survives intact")
}
