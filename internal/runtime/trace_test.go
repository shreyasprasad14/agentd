package runtime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/telemetry"
	"github.com/shreyasprasad/agentd/internal/tools/builtin"
)

// The trace identity an API process would have minted at submission. Fixed
// hex rather than random ids so a failure message names the same trace the
// assertions do.
const (
	testTraceID    = "4bf92f3577b34da6a3ce929d0e0e4736"
	testRootSpanID = "00f067aa0ba902b7"
)

// TestTraceAttemptStepModelAndTool is tracing step one end to end: a whole
// fake-model run, every span of it inside the trace the run row carries and
// under the root span id no worker in this test ever emitted.
func TestTraceAttemptStepModelAndTool(t *testing.T) {
	tracer, rec := recorder(t)
	f := newFixture(t, fake.New(
		deadlineCall("t1", 30),
		finishCall("t2", "traced"),
	).WithPrice(model.Price{InputPerMTok: 3 * model.MicroUSD, OutputPerMTok: 15 * model.MicroUSD}))

	id := f.submitTraced("trace a whole run", runOpts{
		tools: []string{builtin.DeadlineName, builtin.FinishName},
	}, testTraceID, testRootSpanID)
	stop := f.startTracedWorker(tracer, "worker-traced", 30*time.Second)
	run := f.waitTerminal(id)
	// Stop before reading: the attempt span ends when Execute returns, which
	// is after the terminal event the wait above is watching for.
	stop()
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	spans := rec.Ended()
	require.NotEmpty(t, spans)
	wantTrace, err := oteltrace.TraceIDFromHex(testTraceID)
	require.NoError(t, err)
	for _, s := range spans {
		require.Equal(t, wantTrace, s.SpanContext().TraceID(),
			"%s landed outside the run's stored trace", s.Name())
	}

	// The attempt, hanging off the parent rebuilt from the row. It is remote
	// because nothing in this process started it: that is the whole claim of
	// ADR-24, and it is what makes two workers' spans one trace.
	attempt := onlySpan(t, spans, telemetry.SpanAttempt)
	require.Equal(t, testRootSpanID, attempt.Parent().SpanID().String())
	require.True(t, attempt.Parent().IsRemote())
	at := attrs(attempt)
	require.Equal(t, id.String(), at[telemetry.AttrRunID].AsString())
	require.Equal(t, "worker-traced", at[telemetry.AttrWorkerOwner].AsString())
	require.False(t, at[telemetry.AttrRunResumed].AsBool())
	require.Equal(t, int64(0), at[telemetry.AttrEventsSeen].AsInt64())
	require.Equal(t, "finished", at[telemetry.AttrAttemptOutcome].AsString())

	// Four iterations: call the model, run what it asked for, call it again,
	// run the finish tool. Each pair shares a step number because a step is
	// the model call plus the tool calls it requested.
	steps := spansNamed(spans, telemetry.SpanStep)
	require.Len(t, steps, 4)
	var stepNumbers []int64
	for _, s := range steps {
		require.Equal(t, attempt.SpanContext().SpanID(), s.Parent().SpanID())
		stepNumbers = append(stepNumbers, attrs(s)[telemetry.AttrStep].AsInt64())
	}
	require.Equal(t, []int64{1, 1, 2, 2}, stepNumbers)

	models := spansNamed(spans, telemetry.SpanModel)
	require.Len(t, models, 2)
	for _, s := range models {
		require.True(t, isChildOfAny(s, steps), "model.complete must nest under agent.step")
		a := attrs(s)
		require.Equal(t, "fake", a[telemetry.AttrGenAISystem].AsString())
		require.Equal(t, "fake-model", a[telemetry.AttrGenAIRequestModel].AsString())
		require.Equal(t, usage.InputTokens, a[telemetry.AttrGenAIInputTokens].AsInt64())
		require.Equal(t, usage.OutputTokens, a[telemetry.AttrGenAIOutputTokens].AsInt64())
		require.Equal(t, int64(0), a[telemetry.AttrCacheReadTokens].AsInt64())
		require.Equal(t, int64(0), a[telemetry.AttrCacheWriteTokens].AsInt64())
		require.Equal(t, model.StopToolUse, a[telemetry.AttrStopReason].AsString())
		// One bar for the call, with the try count on it rather than one bar
		// per try — the fake answers first time, so that count is 1.
		require.Equal(t, int64(1), a[telemetry.AttrAttempt].AsInt64())
		// 100 input at $3/Mtok plus 10 output at $15/Mtok.
		require.Equal(t, int64(450), a[telemetry.AttrCostMicroUSD].AsInt64())
	}

	tools := spansNamed(spans, telemetry.SpanTool)
	require.Len(t, tools, 2)
	var toolNames []string
	for _, s := range tools {
		require.True(t, isChildOfAny(s, steps), "tool.invoke must nest under agent.step")
		a := attrs(s)
		toolNames = append(toolNames, a[telemetry.AttrToolName].AsString())
		require.Equal(t, "builtin", a[telemetry.AttrToolTrustTier].AsString())
		require.Equal(t, telemetry.OutcomeSucceeded, a[telemetry.AttrToolOutcome].AsString())
		require.False(t, a[telemetry.AttrToolReplayed].AsBool())
		require.Equal(t, int64(0), a[telemetry.AttrToolExitCode].AsInt64())
		require.Positive(t, a[telemetry.AttrSeq].AsInt64(), "the seq is the call's idempotency key")
	}
	require.Equal(t, []string{builtin.DeadlineName, builtin.FinishName}, toolNames)
}

// TestTraceResumedRunSharesStoredTraceID is the property in-process
// propagation cannot have: a run executed by two workers, in two attempts
// separated by a dead process, still draws one trace.
func TestTraceResumedRunSharesStoredTraceID(t *testing.T) {
	tracerA, recA := recorder(t)
	tracerB, recB := recorder(t)
	f := newFixture(t, fake.New(
		deadlineCall("t1", 30),
		slowCall("t2"),
		finishCall("t3", "done after resume"),
	))
	const lease = 2 * time.Second

	id := f.submitTraced("survive a crash, keep the trace", runOpts{}, testTraceID, testRootSpanID)
	stopA := f.startTracedWorker(tracerA, "worker-a", lease)
	f.slow.waitStarted(t)
	stopA()

	stopB := f.startTracedWorker(tracerB, "worker-b", lease)
	defer stopB()
	f.slow.waitStarted(t) // B re-executes the interrupted call
	close(f.slow.release)
	run := f.waitTerminal(id)
	stopB()
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	wantTrace, err := oteltrace.TraceIDFromHex(testTraceID)
	require.NoError(t, err)
	for _, spans := range [][]sdktrace.ReadOnlySpan{recA.Ended(), recB.Ended()} {
		require.NotEmpty(t, spans)
		for _, s := range spans {
			require.Equal(t, wantTrace, s.SpanContext().TraceID(),
				"%s landed outside the run's stored trace", s.Name())
		}
	}

	// Both attempts point at the same stored root, which is what puts them
	// side by side in one waterfall rather than in two.
	attemptA := onlySpan(t, recA.Ended(), telemetry.SpanAttempt)
	attemptB := onlySpan(t, recB.Ended(), telemetry.SpanAttempt)
	require.Equal(t, testRootSpanID, attemptA.Parent().SpanID().String())
	require.Equal(t, testRootSpanID, attemptB.Parent().SpanID().String())
	require.NotEqual(t, attemptA.SpanContext().SpanID(), attemptB.SpanContext().SpanID())

	a, b := attrs(attemptA), attrs(attemptB)
	require.Equal(t, "shutdown", a[telemetry.AttrAttemptOutcome].AsString())
	require.False(t, a[telemetry.AttrRunResumed].AsBool())
	require.Equal(t, int64(0), a[telemetry.AttrEventsSeen].AsInt64())
	// The worker going away is not the run failing: the attempt records the
	// context error but keeps an unset status, so a resumed run is not red.
	require.Equal(t, codes.Unset, attemptA.Status().Code)

	require.Equal(t, "finished", b[telemetry.AttrAttemptOutcome].AsString())
	require.True(t, b[telemetry.AttrRunResumed].AsBool())
	require.Positive(t, b[telemetry.AttrEventsSeen].AsInt64(), "B inherited A's log")

	// B re-executed the interrupted call rather than replaying it from the
	// ledger, and its span says so.
	slow := spansNamed(recB.Ended(), telemetry.SpanTool)[0]
	sa := attrs(slow)
	require.Equal(t, "slow", sa[telemetry.AttrToolName].AsString())
	require.Equal(t, telemetry.OutcomeSucceeded, sa[telemetry.AttrToolOutcome].AsString())
	require.False(t, sa[telemetry.AttrToolReplayed].AsBool())
}

// TestTraceCancelledToolCallStillEndsItsSpan covers the path that ends under
// a detached context: a cancel tears off an in-flight tool call, and the span
// for it has to be closed by the goroutine that is unwinding, saying why,
// without painting a run somebody asked to stop as a failure (ADR-25).
func TestTraceCancelledToolCallStillEndsItsSpan(t *testing.T) {
	tracer, rec := recorder(t)
	f := newFixture(t, fake.New(slowCall("t1")))
	id := f.submitTraced("cancel me mid-tool", runOpts{}, testTraceID, testRootSpanID)
	stop := f.startTracedWorker(tracer, "w1", 10*time.Second)
	defer stop()

	f.slow.waitStarted(t)
	ok, err := f.st.RequestCancel(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)

	run := f.waitTerminal(id)
	stop()
	require.Equal(t, runtime.StatusCancelled, run.Status)

	spans := rec.Ended()
	tool := onlySpan(t, spans, telemetry.SpanTool)
	require.Equal(t, telemetry.OutcomeFailed, attrs(tool)[telemetry.AttrToolOutcome].AsString())
	require.Equal(t, codes.Unset, tool.Status().Code, "a cancelled call is an outcome, not a fault")
	require.NotEmpty(t, tool.Events(), "the span records why the call ended")

	// The run was cancelled, not abandoned: every span above the tool closed
	// too, and the attempt reports that it finished the run itself.
	require.Len(t, spansNamed(spans, telemetry.SpanStep), 2)
	attempt := onlySpan(t, spans, telemetry.SpanAttempt)
	require.Equal(t, "finished", attrs(attempt)[telemetry.AttrAttemptOutcome].AsString())
	require.Equal(t, codes.Unset, attempt.Status().Code)
}

// TestTraceRootSpanClosesTheTree is tracing step two: the summary bar the
// worker that wrote the terminal event draws over everything that led to it,
// under the span id the API minted at submission and every attempt has been
// pointing at since.
func TestTraceRootSpanClosesTheTree(t *testing.T) {
	tracer, rec := recorder(t)
	f := newFixture(t, fake.New(
		deadlineCall("t1", 30),
		finishCall("t2", "traced"),
	).WithPrice(model.Price{InputPerMTok: 3 * model.MicroUSD, OutputPerMTok: 15 * model.MicroUSD}))

	tools := []string{builtin.DeadlineName, builtin.FinishName}
	id := f.submitTraced("draw me a root span", runOpts{tools: tools}, testTraceID, testRootSpanID)
	stop := f.startTracedWorker(tracer, "worker-root", 30*time.Second)
	run := f.waitTerminal(id)
	// The terminal event commits before the span that describes it is ended,
	// so the wait above is not a barrier for the recorder; stopping the worker
	// is.
	stop()
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	spans := rec.Ended()
	root := onlySpan(t, spans, telemetry.SpanRun)
	require.Equal(t, testRootSpanID, root.SpanContext().SpanID().String(),
		"the root has to take the stored id, or nothing hangs off it")
	require.Equal(t, testTraceID, root.SpanContext().TraceID().String())
	require.False(t, root.Parent().IsValid(), "the run's root span is a root")

	// Every attempt was drawn before this span existed and still ends up
	// underneath it, which is the whole trick.
	attempt := onlySpan(t, spans, telemetry.SpanAttempt)
	require.Equal(t, root.SpanContext().SpanID(), attempt.Parent().SpanID())

	// The bar covers the run: back-dated to submission, so it includes the
	// queue wait before any worker claimed the run, and still open when the
	// attempt beneath it started.
	require.True(t, root.StartTime().Equal(run.CreatedAt),
		"root starts at created_at: %s vs %s", root.StartTime(), run.CreatedAt)
	require.False(t, root.StartTime().After(attempt.StartTime()))
	require.True(t, root.EndTime().After(attempt.StartTime()))
	// And it closes before its children do, because it is emitted from inside
	// them. That is the second of the plan's three things that look wrong.
	require.True(t, root.EndTime().Before(attempt.EndTime()))

	a := attrs(root)
	require.Equal(t, id.String(), a[telemetry.AttrRunID].AsString())
	require.Equal(t, runtime.StatusSucceeded, a[telemetry.AttrRunStatus].AsString())
	require.Equal(t, int64(2), a[telemetry.AttrRunSteps].AsInt64())
	require.Equal(t, "fake-model", a[telemetry.AttrGenAIRequestModel].AsString())
	require.Equal(t, 2*usage.InputTokens, a[telemetry.AttrGenAIInputTokens].AsInt64())
	require.Equal(t, 2*usage.OutputTokens, a[telemetry.AttrGenAIOutputTokens].AsInt64())
	require.Equal(t, int64(900), a[telemetry.AttrCostMicroUSD].AsInt64(), "two calls at 450 each")
	require.Equal(t, tools, a[telemetry.AttrTools].AsStringSlice())
}

// TestTraceUnfinishedRunHasNoRootSpan asserts the limitation rather than
// leaving it implied: nothing emits agent.run until a terminal event is
// written, so the run you most want to inspect — one still hung, or one whose
// worker was killed — has no summary bar. Its spans are still in the run's
// trace and the deep link still works, which is why this is a degradation and
// not a defect.
func TestTraceUnfinishedRunHasNoRootSpan(t *testing.T) {
	tracer, rec := recorder(t)
	f := newFixture(t, fake.New(slowCall("t1")))

	id := f.submitTraced("die mid-tool", runOpts{}, testTraceID, testRootSpanID)
	stop := f.startTracedWorker(tracer, "worker-doomed", 30*time.Second)
	f.slow.waitStarted(t)
	stop() // a kill -9, as far as the run is concerned

	run, err := f.st.GetRun(context.Background(), id)
	require.NoError(t, err)
	require.Nil(t, run.FinishedAt, "the run has to still be unfinished for this to mean anything")

	spans := rec.Ended()
	require.Empty(t, spansNamed(spans, telemetry.SpanRun), "a run with no terminal event has no root span")
	// The rest of the trace is intact: the attempt and its children are
	// exported and queryable by the stored trace id, parented to a span that
	// does not exist yet and may never.
	attempt := onlySpan(t, spans, telemetry.SpanAttempt)
	require.Equal(t, testTraceID, attempt.SpanContext().TraceID().String())
	require.Equal(t, testRootSpanID, attempt.Parent().SpanID().String())
}

// recorder is a tracer whose spans stay in this process. It is deliberately
// not the global provider: two tests in one binary must not share one.
//
// It installs the same id generator cmd/agentd does, because without it the
// run's root span would be drawn under a random id and every assertion about
// the tree hanging together would pass for the wrong reason.
func recorder(t *testing.T) (oteltrace.Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithIDGenerator(telemetry.NewIDGenerator()),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("runtime_test"), sr
}

// submitTraced is submit with the trace identity the API mints at submission.
// It writes the ids the same way createRun does, because a worker's only
// route to them is the row.
func (f *fixture) submitTraced(goal string, o runOpts, traceID, rootSpanID string) uuid.UUID {
	f.t.Helper()
	if o.tools == nil {
		o.tools = []string{builtin.DeadlineName, builtin.FinishName, "slow"}
	}
	if o.maxSteps == 0 {
		o.maxSteps = 10
	}
	if o.budget == "" {
		o.budget = "1.00"
	}
	cfg, err := json.Marshal(runtime.AgentConfig{Model: o.model, Tools: o.tools, ToolDelayMS: o.toolDelay})
	require.NoError(f.t, err)
	run, err := f.st.CreateRun(context.Background(), store.NewRun{
		Goal:        goal,
		AgentConfig: cfg,
		MaxSteps:    o.maxSteps,
		BudgetUSD:   o.budget,
		TraceID:     traceID,
		RootSpanID:  rootSpanID,
	})
	require.NoError(f.t, err)
	return run.ID
}

// startTracedWorker is startWorker with a tracer, so a test can watch what
// one worker emitted without seeing another worker's spans in the same
// recorder.
func (f *fixture) startTracedWorker(tracer oteltrace.Tracer, owner string, lease time.Duration) (stop func()) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := runtime.NewWorker(f.st, nil, runtime.WorkerConfig{
		Owner:          owner,
		PollInterval:   50 * time.Millisecond,
		LeaseDuration:  lease,
		ReaperInterval: 200 * time.Millisecond,
		Provider:       f.provider,
		Registry:       f.registry,
		DefaultModel:   "fake-model",
		CancelPoll:     f.cancelPoll,
		Tracer:         tracer,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	f.t.Cleanup(cancel)
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		cancel()
		<-done
	}
}

func spansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func onlySpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	got := spansNamed(spans, name)
	require.Len(t, got, 1, "expected exactly one %s span", name)
	return got[0]
}

// isChildOfAny reports whether s hangs off one of parents. Threading matters
// more than any single attribute here: a span whose context did not reach its
// children turns them into orphan roots.
func isChildOfAny(s sdktrace.ReadOnlySpan, parents []sdktrace.ReadOnlySpan) bool {
	for _, p := range parents {
		if s.Parent().SpanID() == p.SpanContext().SpanID() {
			return true
		}
	}
	return false
}

func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value, len(s.Attributes()))
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value
	}
	return m
}

// TestTraceFailedAttemptIsNotLabelledFinished guards a trap in Execute's
// signature. Its Outcome is only meaningful when the error is nil — every
// error return carries OutcomeFinished, because that is the zero value of the
// type and not because anything finished. A deferred span attribute read
// straight off the named return therefore labels a *failed* attempt
// "finished", which is worse than no attribute: it is a value that quietly
// contaminates any query grouping attempts by outcome.
func TestTraceFailedAttemptIsNotLabelledFinished(t *testing.T) {
	// An empty script: the first model call fails, and keeps failing, so the
	// worker ends up writing the terminal event itself.
	f := newFixture(t, fake.New())
	tracer, sr := recorder(t)
	stop := f.startTracedWorker(tracer, "w1", 10*time.Second)
	defer stop()

	id := f.submitTraced("fail me", runOpts{}, testTraceID, testRootSpanID)
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusFailed, run.Status)

	attempt := onlySpan(t, sr.Ended(), telemetry.SpanAttempt)
	require.Equal(t, "failed", attrs(attempt)[telemetry.AttrAttemptOutcome].AsString(),
		"an attempt that returned an error must not report the zero Outcome")
	require.Equal(t, codes.Error, attempt.Status().Code, "and the span itself is an error span")
}
