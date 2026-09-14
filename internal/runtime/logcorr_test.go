package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/telemetry"
)

// syncBuffer is a bytes.Buffer safe to read while a worker is still writing.
// slog serialises writes through one handler, but the test goroutine reading
// the log is a second writer's worth of racing all by itself.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) lines() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// TestLogLinesCarryTheRunsTraceID is the half of §11's log correlation that a
// unit test of the slog handler cannot reach. The handler reads the trace id
// off the *record's* context, and slog's plain Info/Warn/Error pass
// context.Background() — so a handler that works perfectly in isolation still
// produces no trace ids at all unless every call site uses the Context
// variants. That gap is invisible: nothing fails, the lines just never join
// the waterfall, which is the one thing the handler exists to do.
//
// So this test asserts on the join itself, through a real worker, a real
// JSONHandler, and the run's stored trace identity.
func TestLogLinesCarryTheRunsTraceID(t *testing.T) {
	f := newFixture(t, fake.New(deadlineCall("t1", 30), finishCall("t2", "2026-10-15")))
	tracer, _ := recorder(t)

	var buf syncBuffer
	log := slog.New(telemetry.NewHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := runtime.NewWorker(f.st, log, runtime.WorkerConfig{
		Owner:         "w1",
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 10 * time.Second,
		Provider:      f.provider,
		Registry:      f.registry,
		DefaultModel:  "fake-model",
		CancelPoll:    f.cancelPoll,
		Tracer:        tracer,
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()

	id := f.submitTraced("log me into the trace", runOpts{}, testTraceID, testRootSpanID)
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)
	cancel()
	<-done

	lines := f.logLines(&buf)

	// The worker's own line about the run: outside every span the loop
	// creates, and the line the crash demo is read through.
	requireLogged(t, lines, "run claimed", testTraceID, false)

	// Lines from inside the loop's spans carry a span id too, which is what
	// takes you to one bar of the waterfall rather than to the whole trace.
	requireLogged(t, lines, "model responded", testTraceID, true)
	requireLogged(t, lines, "tool succeeded", testTraceID, true)
	requireLogged(t, lines, "run finished", testTraceID, true)
}

// logLines parses the captured log and fails loudly if nothing was captured,
// so a wiring mistake in the test reads as a wiring mistake rather than as
// every assertion below vacuously passing.
func (f *fixture) logLines(buf *syncBuffer) []map[string]any {
	f.t.Helper()
	lines := buf.lines()
	require.NotEmpty(f.t, lines, "the worker logged nothing at all")
	return lines
}

// requireLogged asserts that the line with this message carries the run's
// trace id, and a span id when the line comes from inside a span.
func requireLogged(t *testing.T, lines []map[string]any, msg, traceID string, wantSpan bool) {
	t.Helper()
	for _, line := range lines {
		if line["msg"] != msg {
			continue
		}
		require.Equal(t, traceID, line["trace_id"],
			"%q does not name the run's trace; the call site is probably using Info rather than InfoContext", msg)
		if wantSpan {
			require.NotEmpty(t, line["span_id"], "%q is inside a span and should name it", msg)
		}
		return
	}
	t.Fatalf("no log line with msg %q; the worker logged %d lines", msg, len(lines))
}
