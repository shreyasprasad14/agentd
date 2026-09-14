package evals_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/evals"
	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
)

// logBuilder assembles a well-formed event log by hand, so the assertion
// vocabulary can be tested against logs no run would produce on purpose —
// a tool completed twice, a ledger row left open, a forged envelope.
type logBuilder struct {
	t      *testing.T
	events []store.Event
	calls  []store.ToolCall
	runID  uuid.UUID
	seq    int32
}

func newLog(t *testing.T, tools []string) *logBuilder {
	b := &logBuilder{t: t, runID: uuid.New()}
	return b.append(runtime.EventRunStarted, runtime.RunStartedPayload{
		Goal:        "a goal",
		AgentConfig: runtime.AgentConfig{Model: "fake-model", Tools: tools},
		MaxSteps:    10,
		BudgetUSD:   "1.00",
		Worker:      "test",
	})
}

func (b *logBuilder) append(typ string, payload any) *logBuilder {
	b.t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(b.t, err)
	b.seq++
	b.events = append(b.events, store.Event{RunID: b.runID, Seq: b.seq, Type: typ, Payload: raw})
	return b
}

// call adds a complete tool call: the model asking for it, the request, and
// the result, plus its ledger row.
func (b *logBuilder) call(id, name string, result any) *logBuilder {
	b.t.Helper()
	args := json.RawMessage(`{}`)
	b.append(runtime.EventModelRequested, runtime.ModelRequestedPayload{Step: 1, Model: "fake-model"})
	b.append(runtime.EventModelResponded, runtime.ModelRespondedPayload{
		Step: 1, Provider: "fake", Model: "fake-model", StopReason: model.StopToolUse,
		Usage: model.Usage{InputTokens: 100, OutputTokens: 10},
		Content: []model.ContentBlock{{
			Type: model.BlockToolUse, ToolUseID: id, Name: name, Input: args,
		}},
	})
	b.append(runtime.EventToolRequested, runtime.ToolRequestedPayload{ToolUseID: id, Name: name, Args: args})
	requestedSeq := b.seq
	raw, err := json.Marshal(result)
	require.NoError(b.t, err)
	b.append(runtime.EventToolSucceeded, runtime.ToolSucceededPayload{ToolUseID: id, Name: name, Result: raw})
	b.calls = append(b.calls, store.ToolCall{
		RunID: b.runID, Seq: requestedSeq, ToolName: name, Args: args, Status: store.ToolCallSucceeded,
	})
	return b
}

// finish closes the log with a text answer.
func (b *logBuilder) finish(answer string) *logBuilder {
	b.t.Helper()
	b.append(runtime.EventModelRequested, runtime.ModelRequestedPayload{Step: 2, Model: "fake-model"})
	b.append(runtime.EventModelResponded, runtime.ModelRespondedPayload{
		Step: 2, Provider: "fake", Model: "fake-model", StopReason: model.StopEndTurn,
		Usage:   model.Usage{InputTokens: 120, OutputTokens: 20},
		Content: []model.ContentBlock{{Type: model.BlockText, Text: answer}},
	})
	return b.append(runtime.EventRunFinished, runtime.RunFinishedPayload{
		Status: runtime.StatusSucceeded, FinalAnswer: answer,
	})
}

func (b *logBuilder) evidence() *evals.Evidence {
	b.t.Helper()
	ev, err := b.collect()
	require.NoError(b.t, err)
	return ev
}

func (b *logBuilder) collect() (*evals.Evidence, error) {
	b.t.Helper()
	run := &store.Run{
		ID: b.runID, Status: runtime.StatusSucceeded, Goal: "a goal",
		BudgetUSD: "1.00", SpentUSD: "0.0000", CreatedAt: time.Now(),
	}
	if len(b.events) > 0 {
		last := b.events[len(b.events)-1]
		if last.Type == runtime.EventRunFinished {
			var p runtime.RunFinishedPayload
			require.NoError(b.t, json.Unmarshal(last.Payload, &p))
			run.Status = p.Status
			now := time.Now()
			run.FinishedAt = &now
		}
	}
	return evals.Collect(run, b.events, b.calls, time.Second)
}

// stubResolver answers citation lookups from a fixed set, so the parser and
// the resolver can be tested without Postgres.
type stubResolver map[string]bool

func (s stubResolver) GetChunkByCitation(_ context.Context, sourceID string, ordinal int) (*store.SearchHit, error) {
	key := sourceID + "#" + itoa(ordinal)
	if !s[key] {
		return nil, store.ErrNotFound
	}
	return &store.SearchHit{SourceID: sourceID, Ordinal: ordinal, Content: "text"}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func check(t *testing.T, a evals.Assertions, ev *evals.Evidence, r evals.ChunkResolver) []string {
	t.Helper()
	if r == nil {
		r = stubResolver{}
	}
	fail, _, _, err := evals.Check(context.Background(), r, a, ev)
	require.NoError(t, err)
	return fail
}
