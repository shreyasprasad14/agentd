package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/tools/builtin"
)

func TestLoopHappyPath(t *testing.T) {
	f := newFixture(t, fake.New(
		deadlineCall("t1", 30),
		finishCall("t2", "The deadline is 2026-10-03."),
	))
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("compute the deadline", runOpts{})
	run := f.waitTerminal(id)

	require.Equal(t, runtime.StatusSucceeded, run.Status)
	require.Nil(t, run.LeaseOwner)
	require.Equal(t, int64(200), run.InputTokens)
	require.Equal(t, int64(20), run.OutputTokens)
	require.Equal(t, "0.0000", run.SpentUSD)

	evs := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolSucceeded,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolSucceeded,
		runtime.EventRunFinished,
	}, eventTypes(evs))

	started := decode[runtime.RunStartedPayload](t, evs[0])
	require.Equal(t, "fake-model", started.AgentConfig.Model, "default model filled in")
	require.Equal(t, "w1", started.Worker)

	finished := decode[runtime.RunFinishedPayload](t, evs[9])
	require.Equal(t, "The deadline is 2026-10-03.", finished.FinalAnswer)

	succeeded := decode[runtime.ToolSucceededPayload](t, evs[4])
	var deadline builtin.DeadlineResult
	require.NoError(t, json.Unmarshal(succeeded.Result, &deadline))
	require.Equal(t, "2026-10-03", deadline.Deadline)
	require.False(t, succeeded.Replayed)

	// Ledger agrees with the log.
	calls, err := f.st.ListToolCalls(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, calls, 2)
	require.Equal(t, int32(4), calls[0].Seq)
	require.Equal(t, store.ToolCallSucceeded, calls[0].Status)
	require.Equal(t, int32(8), calls[1].Seq)

	// The second model call saw the full rebuilt conversation with the
	// enveloped tool result.
	reqs := f.provider.Requests()
	require.Len(t, reqs, 2)
	require.Equal(t, runtime.DefaultSystemPrompt, reqs[1].System)
	require.Len(t, reqs[1].Messages, 3)
	res := reqs[1].Messages[2].Content[0]
	require.Equal(t, model.BlockToolResult, res.Type)
	require.Equal(t, "t1", res.ToolUseID)
	require.True(t, strings.HasPrefix(res.Content, `<tool_result tool="compute_deadline" seq=4>`), res.Content)
	require.Len(t, reqs[1].Tools, 3, "allowlisted tool defs are offered")
	require.Equal(t, int32(1), f.deadline.calls.Load())
}

func TestLoopTextOnlyAnswer(t *testing.T) {
	f := newFixture(t, fake.New(fake.Text("just an answer", usage)))
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("say something", runOpts{})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	evs := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted, runtime.EventModelRequested, runtime.EventModelResponded, runtime.EventRunFinished,
	}, eventTypes(evs))
	require.Equal(t, "just an answer", decode[runtime.RunFinishedPayload](t, evs[3]).FinalAnswer)
}

func TestLoopStepLimit(t *testing.T) {
	f := newFixture(t, fake.New(deadlineCall("t1", 1), deadlineCall("t2", 2), deadlineCall("t3", 3)))
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("loop forever", runOpts{maxSteps: 2})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusFailed, run.Status)

	s := f.state(id)
	require.Equal(t, 2, s.Steps)
	require.Contains(t, s.Error, "step limit")
	require.Equal(t, 2, f.provider.Calls())
	require.Equal(t, int32(2), f.deadline.calls.Load(), "the tool of the last step still runs")
}

func TestLoopBudgetExceeded(t *testing.T) {
	// Each call: 100 in @ $5/MTok + 10 out @ $10/MTok = 0.0005 + 0.0001 USD.
	// Pump the price so one call costs 0.60 USD against a 1.00 budget.
	p := fake.New(deadlineCall("t1", 1), deadlineCall("t2", 2), deadlineCall("t3", 3)).
		WithPrice(model.Price{InputPerMTok: 6_000 * model.MicroUSD, OutputPerMTok: 0})
	f := newFixture(t, p)
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("spend money", runOpts{budget: "1.00"})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusBudgetExceeded, run.Status)
	require.Equal(t, "1.2000", run.SpentUSD)

	evs := f.events(id)
	types := eventTypes(evs)
	require.Equal(t, runtime.EventBudgetExceeded, types[len(types)-2])
	require.Equal(t, runtime.EventRunFinished, types[len(types)-1])
	be := decode[runtime.BudgetExceededPayload](t, evs[len(evs)-2])
	require.Equal(t, int64(1_200_000), be.SpentMicroUSD)
	require.Equal(t, int64(1_000_000), be.BudgetMicroUSD)
	require.Equal(t, 2, f.provider.Calls())
}

func TestLoopToolRejectionsGoBackToTheModel(t *testing.T) {
	p := fake.New(
		fake.ToolUse("t1", builtin.DeadlineName, map[string]any{"days": "thirty"}, usage),                      // schema violation
		fake.ToolUse("t2", "nope", map[string]any{}, usage),                                                    // unknown tool
		fake.ToolUse("t3", "slow", map[string]any{}, usage),                                                    // not allowlisted
		fake.ToolUse("t4", builtin.DeadlineName, map[string]any{"start_date": "2026-13-40", "days": 1}, usage), // passes schema, tool errors
		finishCall("t5", "gave up"),
	)
	f := newFixture(t, p)
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("misbehave", runOpts{tools: []string{builtin.DeadlineName, builtin.FinishName}})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	evs := f.events(id)
	require.Equal(t, 4, countType(evs, runtime.EventToolFailed))
	require.Equal(t, 1, countType(evs, runtime.EventToolSucceeded))

	var failures []runtime.ToolFailedPayload
	for _, ev := range evs {
		if ev.Type == runtime.EventToolFailed {
			failures = append(failures, decode[runtime.ToolFailedPayload](t, ev))
		}
	}
	require.Contains(t, failures[0].Error, "invalid arguments")
	require.False(t, failures[0].Retryable)
	require.Contains(t, failures[1].Error, "unknown tool")
	require.Contains(t, failures[2].Error, "not allowed")
	require.Contains(t, failures[3].Error, "start_date")
	require.True(t, failures[3].Retryable)

	// Every failure reached the model as an is_error tool_result.
	reqs := f.provider.Requests()
	require.Len(t, reqs, 5)
	for i := 1; i < 5; i++ {
		last := reqs[i].Messages[len(reqs[i].Messages)-1]
		require.Equal(t, model.RoleUser, last.Role)
		require.True(t, last.Content[0].IsError, "request %d should carry an error result", i)
	}

	calls, err := f.st.ListToolCalls(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, calls, 5)
	require.Equal(t, store.ToolCallFailed, calls[0].Status)
	require.Equal(t, store.ToolCallSucceeded, calls[4].Status)
}

func TestLoopCancelRequested(t *testing.T) {
	f := newFixture(t, fake.New(deadlineCall("t1", 1), deadlineCall("t2", 2)))
	id := f.submit("cancel me", runOpts{})
	ok, err := f.st.RequestCancel(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)

	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusCancelled, run.Status)
	require.Equal(t, []string{
		runtime.EventRunStarted, runtime.EventCancelRequested, runtime.EventRunFinished,
	}, eventTypes(f.events(id)))
	require.Equal(t, 0, f.provider.Calls())
}

func TestLoopModelFailureFailsTheRun(t *testing.T) {
	p := fake.New()
	p.OnComplete = func(context.Context, model.Request, int) error { return errors.New("connection refused") }
	f := newFixture(t, p)
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("no model", runOpts{})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusFailed, run.Status)
	s := f.state(id)
	require.Contains(t, s.Error, "after 3 attempts")
	require.Equal(t, 3, f.provider.Calls())
	// One dangling model_requested, then run_finished.
	require.Equal(t, []string{runtime.EventRunStarted, runtime.EventModelRequested, runtime.EventRunFinished}, eventTypes(f.events(id)))
}
