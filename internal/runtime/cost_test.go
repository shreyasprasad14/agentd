package runtime_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/tools"
)

// rerankCost is roughly what one hosted rerank pass measures at (plan
// Decision 3): ~11,000 input and ~500 output tokens per search. The number
// matters less than its shape — it is large enough that ignoring it would
// misreport a search-heavy run's spend by more than a rounding error, which is
// the reason the attribution exists at all.
var rerankCost = tools.Cost{
	MicroUSD:     41_500,
	InputTokens:  11_000,
	OutputTokens: 500,
	Model:        "claude-sonnet-rerank",
}

// TestToolCostIsAttributedToTheRun is ADR-23's property: model calls that
// happen *inside* a tool move the run's counters exactly as the loop's own
// calls do. Before M4 this spend was invisible, which meant the budget bounded
// the loop rather than the run — a soundness hole, not a cost question.
//
// The model is priced at zero here on purpose, so every micro-USD the run ends
// up with came from inside the tool and nowhere else.
func TestToolCostIsAttributedToTheRun(t *testing.T) {
	f := newFixture(t, fake.New(costlyCall("t1"), finishCall("t2", "found it")))
	f.costly.cost = rerankCost
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("search and answer", runOpts{tools: []string{"costly", "finish"}, budget: "1.00"})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)
	require.Equal(t, int32(1), f.costly.calls.Load())

	// The run's counters moved by the tool's spend even though the free model
	// contributed nothing to the bill.
	require.Equal(t, "0.0415", run.SpentUSD)
	// Two model calls at 100/10 each, plus the tool's own 11,000/500.
	require.Equal(t, int64(11_200), run.InputTokens)
	require.Equal(t, int64(520), run.OutputTokens)

	// The event says the same thing the counters do, because one transaction
	// wrote both.
	evs := f.events(id)
	succeeded := decode[runtime.ToolSucceededPayload](t, evs[4])
	require.Equal(t, "costly", succeeded.Name)
	require.Equal(t, rerankCost.MicroUSD, succeeded.CostMicroUSD)
	require.Equal(t, rerankCost.InputTokens, succeeded.InputTokens)
	require.Equal(t, rerankCost.OutputTokens, succeeded.OutputTokens)
	require.Equal(t, rerankCost.Model, succeeded.CostModel,
		"the billed backend is named, since it is not the one the run's model field reports")

	// And the fold of the log agrees with the row, which is the invariant M1
	// advertised and this milestone had to work to keep: anyone checking
	// spent_usd by summing the log must now sum tool_succeeded as well as
	// model_responded.
	state := f.state(id)
	require.Equal(t, rerankCost.MicroUSD, state.SpentMicroUSD)
	require.Equal(t, int64(11_200), state.InputTokens)
	require.Equal(t, int64(520), state.OutputTokens)

	// A tool's internal prompt is not the loop's prompt, so it must not be
	// used to size the next completion. The last model call measured 100.
	require.Equal(t, int64(100), state.LastInputTokens)
}

// TestToolCostAloneTripsTheBudget is the other half of attribution: once tool
// spend counts, it can end a run by itself, with the model priced at zero and
// never having cost anything.
//
// It also pins the limitation ADR-23 is honest about. Tool cost is committed
// *after* the tool runs, so the budget is a ceiling on model calls and an
// audit on tool calls: this run ends over its budget, by one tool call's
// spend, and that is the documented bound rather than a bug. Pre-flighting it
// would mean the registry predicting each tool's cost before invoking it.
func TestToolCostAloneTripsTheBudget(t *testing.T) {
	f := newFixture(t, fake.New(costlyCall("t1"), finishCall("t2", "never reached")))
	f.costly.cost = tools.Cost{MicroUSD: 150_000, InputTokens: 11_000, OutputTokens: 500, Model: "rerank"}
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("one search is all it takes", runOpts{tools: []string{"costly", "finish"}, budget: "0.10"})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusBudgetExceeded, run.Status)
	require.Equal(t, int32(1), f.costly.calls.Load(), "the tool that spent it ran exactly once")
	require.Equal(t, 1, f.provider.Calls(), "no model call was made after the budget was gone")

	evs := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolSucceeded,
		runtime.EventBudgetExceeded, runtime.EventRunFinished,
	}, eventTypes(evs))

	be := decode[runtime.BudgetExceededPayload](t, evs[5])
	require.Equal(t, runtime.BudgetReasonSpent, be.Reason,
		"a tool overrun is caught by the backstop, not the ceiling: the money was gone before the loop looked")
	require.Equal(t, int64(150_000), be.SpentMicroUSD)
	require.Equal(t, int64(100_000), be.BudgetMicroUSD)

	// The overshoot, stated rather than hidden. It is bounded by one tool
	// call's spend.
	require.Equal(t, "0.1500", run.SpentUSD)
}

// TestRunThatFitsItsBudgetCompletes is the control for the two budget tests
// that terminate early: with a real price and room to spare, nothing about the
// pre-flight check interferes with a run that can afford itself. A ceiling
// that also stopped affordable runs would pass every other test here.
func TestRunThatFitsItsBudgetCompletes(t *testing.T) {
	// 100 in @ $3/MTok + 10 out @ $15/MTok = 450 micro-USD per call, and the
	// fake caps output at 4,000 tokens, so the worst case of a call is
	// ~60,000 micro-USD against a 1.00 budget.
	p := fake.New(deadlineCall("t1", 30), finishCall("t2", "2026-10-15")).
		WithPrice(model.Price{InputPerMTok: 3 * model.MicroUSD, OutputPerMTok: 15 * model.MicroUSD}).
		WithMaxOutputTokens(4_000)
	f := newFixture(t, p)
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("a run that can afford itself", runOpts{budget: "1.00"})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)
	require.Equal(t, 2, f.provider.Calls(), "both calls were made")

	state := f.state(id)
	require.Equal(t, 2, state.Steps)
	require.Zero(t, countType(f.events(id), runtime.EventBudgetExceeded))

	// The exit criterion, as a number: a terminated-or-not run never ends up
	// past its ceiling.
	require.Less(t, state.SpentMicroUSD, state.BudgetMicroUSD)
	// Two calls at 450 micro-USD each, folded from the log exactly.
	require.Equal(t, int64(900), state.SpentMicroUSD)

	// The column reads 0.0010 rather than 0.0009, and that is not a bug in the
	// accounting: spent_usd is NUMERIC(10,4), so each increment is rounded to a
	// hundredth of a cent as it is added — 0.00045 twice becomes 0.0005 twice.
	// It matters that this is the *denormalised* figure. Budget enforcement
	// reads State.SpentMicroUSD, which is the integer fold of the log and is
	// exact; the column is the cheap answer for a client that did not ask for
	// the log, and a run spending in half-micro-dollar steps is not one whose
	// ceiling turns on the fourth decimal place.
	require.Equal(t, "0.0010", run.SpentUSD)
}
