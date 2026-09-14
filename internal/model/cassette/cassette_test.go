package cassette_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/cassette"
	"github.com/shreyasprasad/agentd/internal/model/fake"
)

// claudePrice is a rate card with all four rates distinct, so a bug that
// prices output at the input rate cannot pass.
var claudePrice = model.Price{
	InputPerMTok: 15_000_000, OutputPerMTok: 75_000_000,
	CacheReadPerMTok: 1_500_000, CacheWritePerMTok: 18_750_000,
}

func record(t *testing.T, live model.Provider, reqs ...model.Request) *cassette.File {
	t.Helper()
	f := cassette.NewFile("case", live, "claude-opus-5", nil)
	p, err := cassette.New(cassette.Config{File: f, OnMiss: cassette.MissRecord, Live: live})
	require.NoError(t, err)
	for _, req := range reqs {
		_, err := p.Complete(context.Background(), req)
		require.NoError(t, err)
	}
	return p.File()
}

func replay(t *testing.T, f *cassette.File) *cassette.Provider {
	t.Helper()
	p, err := cassette.New(cassette.Config{File: f})
	require.NoError(t, err)
	return p
}

func TestReplayReturnsTheRecordedResponse(t *testing.T) {
	live := fake.New(fake.Text("first", model.Usage{InputTokens: 10, OutputTokens: 2}),
		fake.Text("second", model.Usage{InputTokens: 20, OutputTokens: 3}))
	one := request(toolResult(1, `{"a":1}`))
	two := request(toolResult(2, `{"b":2}`))
	f := record(t, live, one, two)
	require.Len(t, f.Entries, 2)

	p := replay(t, f)
	first, err := p.Complete(context.Background(), one)
	require.NoError(t, err)
	require.Equal(t, "first", first.Text())
	second, err := p.Complete(context.Background(), two)
	require.NoError(t, err)
	require.Equal(t, "second", second.Text())
	require.Equal(t, int64(20), second.Usage.InputTokens, "usage replays, so cost accounting does too")
}

// TestReplayRepeatsAnIdenticalRequest is the whole reason the key is a hash.
// A worker that dies between model_requested and model_responded leaves a
// dangling request the next worker repeats; under ordinal matching that repeat
// would consume the next entry and desynchronise the rest of the run.
func TestReplayRepeatsAnIdenticalRequest(t *testing.T) {
	live := fake.New(fake.Text("first", model.Usage{}), fake.Text("second", model.Usage{}))
	one := request(toolResult(1, `{"a":1}`))
	two := request(toolResult(2, `{"b":2}`))
	f := record(t, live, one, two)

	p := replay(t, f)
	for i := 0; i < 3; i++ {
		resp, err := p.Complete(context.Background(), one)
		require.NoError(t, err)
		require.Equal(t, "first", resp.Text(), "attempt %d", i+1)
	}
	resp, err := p.Complete(context.Background(), two)
	require.NoError(t, err)
	require.Equal(t, "second", resp.Text())
}

func TestReplayNeverCallsTheLiveProviderOnAMiss(t *testing.T) {
	live := fake.New(fake.Text("recorded", model.Usage{}))
	f := record(t, live, request(toolResult(1, `{"a":1}`)))

	// A second fake standing in for the real API. Under MissFail it must not
	// be reached, whatever the cassette does not know.
	forbidden := fake.New(fake.Text("live", model.Usage{}))
	p, err := cassette.New(cassette.Config{File: f, Live: forbidden}) // OnMiss defaults to fail
	require.NoError(t, err)

	_, err = p.Complete(context.Background(), request(toolResult(1, `{"changed":true}`)))
	require.Error(t, err)
	require.Equal(t, 0, forbidden.Calls(), "a miss must never reach a live provider")

	var miss *cassette.MissError
	require.ErrorAs(t, err, &miss)
	require.Equal(t, 1, miss.Entries)
	require.NotNil(t, miss.Nearest)
	require.Contains(t, err.Error(), "message 0", "the miss names where the drift is")
	require.Contains(t, err.Error(), "volatile")
}

func TestMissErrorNamesAChangedSystemPrompt(t *testing.T) {
	live := fake.New(fake.Text("recorded", model.Usage{}))
	f := record(t, live, request(toolResult(1, `{"a":1}`)))

	drifted := request(toolResult(1, `{"a":1}`))
	drifted.System = "a different system prompt"
	_, err := replay(t, f).Complete(context.Background(), drifted)
	require.ErrorContains(t, err, "system prompt changed")
}

func TestMissErrorNamesAnExtraStep(t *testing.T) {
	live := fake.New(fake.Text("recorded", model.Usage{}))
	base := request(toolResult(1, `{"a":1}`))
	f := record(t, live, base)

	longer := request(toolResult(1, `{"a":1}`),
		model.Message{Role: model.RoleAssistant, Content: []model.ContentBlock{{Type: model.BlockText, Text: "hm"}}})
	_, err := replay(t, f).Complete(context.Background(), longer)
	require.ErrorContains(t, err, "longer than the recording")
}

// TestRecordedPriceReplaysWithoutAKey is what makes the BUDGET category exist
// in replay mode: the pre-flight ceiling (ADR-22) prices a call that has not
// happened yet, so it needs a rate card and not a recorded number.
func TestRecordedPriceReplaysWithoutAKey(t *testing.T) {
	live := fake.New(fake.Text("x", model.Usage{})).WithPrice(claudePrice).WithMaxOutputTokens(16000)
	f := record(t, live, request(toolResult(1, `{"a":1}`)))

	p := replay(t, f)
	usage := model.Usage{InputTokens: 2_000, OutputTokens: 500, CacheReadInputTokens: 1_000, CacheCreationInputTokens: 100}
	require.Equal(t, live.CostMicroUSD("claude-opus-5", usage), p.CostMicroUSD("claude-opus-5", usage))
	require.Equal(t, 16000, p.MaxOutputTokens("claude-opus-5"))
	require.Equal(t, claudePrice.OutputPerMTok, f.Price.OutputPerMTok, "each rate is probed independently")
}

func TestProviderNameIsTheRecordedBackend(t *testing.T) {
	live := fake.New(fake.Text("x", model.Usage{})).WithName("anthropic")
	f := record(t, live, request(toolResult(1, `{"a":1}`)))
	require.Equal(t, "anthropic", replay(t, f).Name(),
		"a replayed run's event log should name the backend the trajectory came from")
}

func TestRecordingAppendsToAnExistingCassette(t *testing.T) {
	live := fake.New(fake.Text("one", model.Usage{}), fake.Text("two", model.Usage{}))
	one := request(toolResult(1, `{"a":1}`))
	f := record(t, live, one)

	p, err := cassette.New(cassette.Config{File: f, OnMiss: cassette.MissRecord, Live: live})
	require.NoError(t, err)
	// The call already on file must not be re-asked; only the new one is.
	_, err = p.Complete(context.Background(), one)
	require.NoError(t, err)
	_, err = p.Complete(context.Background(), request(toolResult(2, `{"b":2}`)))
	require.NoError(t, err)

	hits, _, recorded := p.Stats()
	require.Equal(t, 1, hits)
	require.Equal(t, 1, recorded)
	require.Len(t, p.File().Entries, 2)
	require.Equal(t, 2, live.Calls(), "re-recording only re-asks the calls that actually changed")
}

func TestSaveLoadRoundTrip(t *testing.T) {
	live := fake.New(fake.ToolUse("tu_1", "finish", map[string]any{"answer": "42"},
		model.Usage{InputTokens: 7, OutputTokens: 1})).WithPrice(claudePrice).WithMaxOutputTokens(8000)
	f := record(t, live, request(toolResult(1, `{"a":1}`)))
	f.Volatile = append(f.Volatile, "elapsed_ns")

	path := filepath.Join(t.TempDir(), "nested", "case.json")
	require.NoError(t, f.Save(path))

	got, err := cassette.Load(path)
	require.NoError(t, err)
	require.Equal(t, f.Provider, got.Provider)
	require.Equal(t, f.Price, got.Price)
	require.Equal(t, f.MaxOutputTokens, got.MaxOutputTokens)
	require.Equal(t, f.Volatile, got.Volatile)
	require.Len(t, got.Entries, 1)
	require.Equal(t, "finish", got.Entries[0].Response.ToolUses()[0].Name)

	// And it still replays the call it was recorded from.
	resp, err := replay(t, got).Complete(context.Background(), request(toolResult(1, `{"a":1}`)))
	require.NoError(t, err)
	require.Equal(t, model.StopToolUse, resp.StopReason)
}

func TestLoadRejectsAForeignVersion(t *testing.T) {
	live := fake.New(fake.Text("x", model.Usage{}))
	f := record(t, live, request(toolResult(1, `{"a":1}`)))
	f.Version = 99
	path := filepath.Join(t.TempDir(), "case.json")
	require.NoError(t, f.Save(path))

	_, err := cassette.Load(path)
	require.ErrorContains(t, err, "eval-record")
}

func TestNewRejectsDuplicateKeys(t *testing.T) {
	live := fake.New(fake.Text("x", model.Usage{}))
	f := record(t, live, request(toolResult(1, `{"a":1}`)))
	f.Entries = append(f.Entries, f.Entries[0])
	_, err := cassette.New(cassette.Config{File: f})
	require.ErrorContains(t, err, "duplicate entry")
}

func TestRecordModeNeedsALiveProvider(t *testing.T) {
	_, err := cassette.New(cassette.Config{File: &cassette.File{Version: cassette.Version}, OnMiss: cassette.MissRecord})
	require.ErrorContains(t, err, "live provider")
}
