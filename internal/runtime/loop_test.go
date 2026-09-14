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

// TestLoopBudgetExceeded is the ceiling rather than the receipt: the run stops
// before the call it cannot afford, so it terminates with its budget intact
// instead of with the overrun already on the books (ADR-22). Through M3 this
// test asserted the opposite — a run that ended 20% past a 1.00 budget — which
// is the audit this milestone replaced.
func TestLoopBudgetExceeded(t *testing.T) {
	// Each call: 100 in @ $5/MTok + 10 out @ $10/MTok = 0.0005 + 0.0001 USD.
	// Pump the price so one call costs 0.60 USD against a 1.00 budget, which
	// also puts the first call's worst case — the whole prompt, tool schemas
	// included — over the budget on its own.
	p := fake.New(deadlineCall("t1", 1), deadlineCall("t2", 2), deadlineCall("t3", 3)).
		WithPrice(model.Price{InputPerMTok: 6_000 * model.MicroUSD, OutputPerMTok: 0})
	f := newFixture(t, p)
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("spend money", runOpts{budget: "1.00"})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusBudgetExceeded, run.Status)
	require.Equal(t, "0.0000", run.SpentUSD, "refused before the money was spent")
	require.Equal(t, 0, f.provider.Calls(), "the refused call never reached the provider")

	evs := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted, runtime.EventBudgetExceeded, runtime.EventRunFinished,
	}, eventTypes(evs))

	be := decode[runtime.BudgetExceededPayload](t, evs[1])
	require.Equal(t, runtime.BudgetReasonWouldExceed, be.Reason)
	require.Equal(t, int64(0), be.SpentMicroUSD)
	require.Equal(t, int64(1_000_000), be.BudgetMicroUSD)
	require.Greater(t, be.EstimateMicroUSD, be.BudgetMicroUSD, "the refused call is priced above the whole budget")
	// A refusal has to be arguable, so the payload carries the arithmetic
	// rather than a verdict: at 6_000 µUSD per input token and nothing for
	// output, the token count it reports reproduces the price it quotes.
	require.Equal(t, be.EstimatedInputTokens*6_000, be.EstimateMicroUSD)
	require.Zero(t, be.MaxOutputTokens, "the fake names no cap, so the estimate stands on its input term")
	require.Contains(t, f.state(id).Error, "next call estimated at")
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

// TestLoopRoutesByModel is the M1.5 shape: one worker, two backends behind a
// Router, and agent_config.model deciding which one a run talks to. The
// event log names the real backend and the run is priced at that backend's
// rates, with no change to the loop beyond reading Response.Provider.
func TestLoopRoutesByModel(t *testing.T) {
	local := fake.New(finishCall("t1", "from local")).WithName("local")
	claude := fake.New(finishCall("t2", "from claude")).WithName("anthropic").
		WithPrice(model.Price{InputPerMTok: 5_000_000}) // $5/MTok in, nothing out: 100 tokens = 500 µUSD
	router := model.NewRouter().
		Register("local", local, nil).
		Register("anthropic", claude, func(m string) bool { return strings.HasPrefix(m, "claude-") })

	f := newFixture(t, local)
	stop := f.startWorkerWith(router, "w1", 10*time.Second)
	defer stop()

	hosted := f.submit("go hosted", runOpts{model: "anthropic/claude-opus-5"})
	onBox := f.submit("stay local", runOpts{})

	hostedRun := f.waitTerminal(hosted)
	onBoxRun := f.waitTerminal(onBox)
	require.Equal(t, runtime.StatusSucceeded, hostedRun.Status)
	require.Equal(t, runtime.StatusSucceeded, onBoxRun.Status)
	require.Equal(t, "0.0005", hostedRun.SpentUSD, "priced at the hosted backend's rate")
	require.Equal(t, "0.0000", onBoxRun.SpentUSD)

	hostedResp := decode[runtime.ModelRespondedPayload](t, f.events(hosted)[2])
	require.Equal(t, "anthropic", hostedResp.Provider)
	require.Equal(t, "claude-opus-5", hostedResp.Model, "prefix stripped before the backend saw it")
	require.Equal(t, int64(500), hostedResp.CostMicroUSD)
	require.Equal(t, "from claude", f.state(hosted).FinalAnswer)

	onBoxResp := decode[runtime.ModelRespondedPayload](t, f.events(onBox)[2])
	require.Equal(t, "local", onBoxResp.Provider)
	require.Equal(t, "fake-model", onBoxResp.Model, "worker default model, routed to the default backend")
	require.Equal(t, "from local", f.state(onBox).FinalAnswer)

	require.Equal(t, 1, claude.Calls())
	require.Equal(t, 1, local.Calls())
	require.Equal(t, "claude-opus-5", claude.Requests()[0].Model)
	require.Equal(t, "fake-model", local.Requests()[0].Model)
}

// TestLoopEmptyModelResponseIsRetried: a response with no text and no tool
// call is what an OpenAI-compatible server returns when it drops a tool call
// it could not parse. It must be retried like a transport error, not
// recorded as a successful run with an empty answer.
func TestLoopEmptyModelResponseIsRetried(t *testing.T) {
	empty := &model.Response{StopReason: model.StopEndTurn, Usage: model.Usage{InputTokens: 500, OutputTokens: 180}}
	f := newFixture(t, fake.New(empty, finishCall("t1", "recovered")))
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("flaky model", runOpts{})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)
	require.Equal(t, "recovered", f.state(id).FinalAnswer)
	require.Equal(t, 2, f.provider.Calls())
	// The retry happens inside one model step: a single model_requested.
	require.Equal(t, []string{
		runtime.EventRunStarted, runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolSucceeded, runtime.EventRunFinished,
	}, eventTypes(f.events(id)))

	// Persistently empty: the run fails and says why, instead of succeeding
	// with nothing.
	f2 := newFixture(t, fake.New(empty, empty, empty))
	stop2 := f2.startWorker("w2", 10*time.Second)
	defer stop2()
	id2 := f2.submit("always empty", runOpts{})
	run2 := f2.waitTerminal(id2)
	require.Equal(t, runtime.StatusFailed, run2.Status)
	require.Contains(t, f2.state(id2).Error, "empty response")
	require.Equal(t, 3, f2.provider.Calls())
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
