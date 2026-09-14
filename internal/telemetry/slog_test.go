package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// jsonLogger builds the production shape — the wrapper around a real
// slog.JSONHandler — and returns the parsed line it wrote. Asserting on
// parsed JSON rather than on a fake handler's captured attrs is deliberate:
// what ships is a JSON handler, and a wrapper that behaves differently once
// groups and With() have been through it would pass any other test.
func jsonLogger(t *testing.T, opts *slog.HandlerOptions) (*slog.Logger, func() []map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(NewHandler(slog.NewJSONHandler(&buf, opts)))
	return log, func() []map[string]any {
		t.Helper()
		var lines []map[string]any
		dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
		for dec.More() {
			line := map[string]any{}
			require.NoError(t, dec.Decode(&line))
			lines = append(lines, line)
		}
		return lines
	}
}

func TestHandler_AddsTheIDsFromTheRecordContext(t *testing.T) {
	tel, _ := recorder(t)
	log, lines := jsonLogger(t, nil)

	ctx, span := tel.Tracer().Start(context.Background(), SpanStep)
	defer End(span, nil)
	log.InfoContext(ctx, "step finished", "run_id", "r-1", "seq", 7)

	got := lines()
	require.Len(t, got, 1)
	require.Equal(t, span.SpanContext().TraceID().String(), got[0]["trace_id"])
	require.Equal(t, span.SpanContext().SpanID().String(), got[0]["span_id"])
	// The line the caller wrote survives intact; the wrapper only adds.
	require.Equal(t, "step finished", got[0]["msg"])
	require.Equal(t, "r-1", got[0]["run_id"])
}

func TestHandler_NoSpanNoEmptyFields(t *testing.T) {
	// agentd ingest and agentd eval log this way for whole minutes.
	log, lines := jsonLogger(t, nil)

	log.InfoContext(context.Background(), "chunked document", "chunks", 42)
	log.Info("no context at all")

	got := lines()
	require.Len(t, got, 2)
	for _, line := range got {
		require.NotContains(t, line, "trace_id", "an untraced line must not carry an empty id")
		require.NotContains(t, line, "span_id")
	}
}

func TestHandler_CorrelatesUnderARemoteParent(t *testing.T) {
	// A worker logs its claim before it starts a span of its own, and those
	// lines belong to the run's trace just as much as the later ones.
	tid, sid := NewIDs()
	log, lines := jsonLogger(t, nil)

	log.InfoContext(RemoteParent(context.Background(), tid.String(), sid.String()), "claimed run")

	got := lines()
	require.Len(t, got, 1)
	require.Equal(t, tid.String(), got[0]["trace_id"])
	require.Equal(t, sid.String(), got[0]["span_id"])
}

func TestHandler_WithAttrsKeepsBoth(t *testing.T) {
	tel, _ := recorder(t)
	log, lines := jsonLogger(t, nil)

	ctx, span := tel.Tracer().Start(context.Background(), SpanTool)
	defer End(span, nil)
	// How every logger in this codebase is built: one .With() at
	// construction, then context-carrying calls forever after.
	log.With("mode", "work").With("run_id", "r-2").InfoContext(ctx, "tool invoked")

	got := lines()
	require.Len(t, got, 1)
	require.Equal(t, "work", got[0]["mode"], "WithAttrs must wrap the inner handler's result, not replace it")
	require.Equal(t, "r-2", got[0]["run_id"])
	require.Equal(t, span.SpanContext().TraceID().String(), got[0]["trace_id"])
	require.Equal(t, span.SpanContext().SpanID().String(), got[0]["span_id"])
}

func TestHandler_WithGroupKeepsBoth(t *testing.T) {
	tel, _ := recorder(t)
	log, lines := jsonLogger(t, nil)

	ctx, span := tel.Tracer().Start(context.Background(), SpanCreateRun)
	defer End(span, nil)
	log.WithGroup("http").With("route", "/v1/runs").InfoContext(ctx, "handled")

	got := lines()
	require.Len(t, got, 1)
	group, ok := got[0]["http"].(map[string]any)
	require.True(t, ok, "the group the caller opened must survive: %v", got[0])
	require.Equal(t, "/v1/runs", group["route"])
	// Qualified by the group, because record attributes are: the ids are
	// added to the record, and that is what slog does with those.
	require.Equal(t, span.SpanContext().TraceID().String(), group["trace_id"])
	require.Equal(t, span.SpanContext().SpanID().String(), group["span_id"])
}

func TestHandler_EnabledDelegates(t *testing.T) {
	inner := slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewHandler(inner)

	require.False(t, h.Enabled(context.Background(), slog.LevelInfo), "the level belongs to the inner handler")
	require.True(t, h.Enabled(context.Background(), slog.LevelError))
	// And it keeps belonging to it through a derived handler.
	require.False(t, h.WithAttrs([]slog.Attr{slog.String("mode", "work")}).Enabled(context.Background(), slog.LevelDebug))
}

func TestHandler_LeavesTheCallersRecordAlone(t *testing.T) {
	// A Record can be handed to more than one handler, so the ids have to go
	// on a copy: handling the same record twice must produce two identical
	// lines rather than a second one carrying the first one's additions.
	tel, _ := recorder(t)
	ctx, span := tel.Tracer().Start(context.Background(), SpanModel)
	defer End(span, nil)

	var buf bytes.Buffer
	h := NewHandler(slog.NewJSONHandler(&buf, nil))
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "completed", 0)
	rec.AddAttrs(slog.String("run_id", "r-3"))

	require.NoError(t, h.Handle(ctx, rec))
	require.NoError(t, h.Handle(ctx, rec))

	require.Equal(t, 1, rec.NumAttrs(), "the record the caller still holds gained nothing")
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for range 2 {
		line := map[string]any{}
		require.NoError(t, dec.Decode(&line))
		require.Equal(t, "r-3", line["run_id"])
		require.Equal(t, span.SpanContext().TraceID().String(), line["trace_id"])
	}
}
