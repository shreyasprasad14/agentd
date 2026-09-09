package python

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/sandbox"
	"github.com/shreyasprasad/agentd/internal/tools"
)

// fakeExecutor records the spec and returns a scripted output.
type fakeExecutor struct {
	spec sandbox.Spec
	out  *sandbox.Output
	err  error
}

func (f *fakeExecutor) Run(_ context.Context, spec sandbox.Spec) (*sandbox.Output, error) {
	f.spec = spec
	return f.out, f.err
}

func invoke(t *testing.T, tool *Tool, args string) (tools.Result, error) {
	t.Helper()
	return tool.Invoke(context.Background(), tools.Invocation{
		RunID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		Seq:   7,
		Args:  json.RawMessage(args),
	})
}

func TestInvokeShapesSpecAndResult(t *testing.T) {
	ex := &fakeExecutor{out: &sandbox.Output{
		Stdout: []byte("42\n"), Stderr: []byte("warn\n"), ExitCode: 0, Duration: 1234 * time.Millisecond,
	}}
	tool := New(ex, sandbox.DefaultLimits())

	res, err := invoke(t, tool, `{"code":"print(6*7)","stdin":"in","files":{"a.txt":"A"},"timeout_seconds":5}`)
	require.NoError(t, err)

	require.Equal(t, []string{"python3", "/work/in/main.py"}, ex.spec.Cmd)
	require.Equal(t, "print(6*7)", string(ex.spec.Files["main.py"]))
	require.Equal(t, "A", string(ex.spec.Files["a.txt"]))
	require.Equal(t, "in", string(ex.spec.Stdin))
	require.Equal(t, 5*time.Second, ex.spec.Timeout)
	require.Equal(t, map[string]string{
		sandbox.LabelRunID: "11111111-2222-3333-4444-555555555555",
		sandbox.LabelSeq:   "7",
		sandbox.LabelTool:  "run_python",
	}, ex.spec.Labels)

	var got Result
	require.NoError(t, json.Unmarshal(res.Content, &got))
	require.Equal(t, Result{Stdout: "42\n", Stderr: "warn\n", DurationMS: 1234}, got)
	require.Equal(t, 0, res.ExitCode)
	require.False(t, res.Terminal)
}

func TestInvokeTimeoutDefaultsAndClamps(t *testing.T) {
	ex := &fakeExecutor{out: &sandbox.Output{}}
	limits := sandbox.Limits{DefaultTimeout: 3 * time.Second, MaxTimeout: 10 * time.Second}
	tool := New(ex, limits)

	_, err := invoke(t, tool, `{"code":"pass"}`)
	require.NoError(t, err)
	require.Equal(t, 3*time.Second, ex.spec.Timeout, "no timeout_seconds → default")

	_, err = invoke(t, tool, `{"code":"pass","timeout_seconds":600}`)
	require.NoError(t, err)
	require.Equal(t, 10*time.Second, ex.spec.Timeout, "above max → clamped (the schema rejects it first, but Invoke must not trust that)")
}

func TestInvokeNonzeroExitIsAResultNotAnError(t *testing.T) {
	ex := &fakeExecutor{out: &sandbox.Output{
		Stderr: []byte("Traceback...\nZeroDivisionError\n"), ExitCode: 1, Duration: time.Second,
	}}
	res, err := invoke(t, New(ex, sandbox.DefaultLimits()), `{"code":"1/0"}`)
	require.NoError(t, err)
	require.Equal(t, 1, res.ExitCode)
	var got Result
	require.NoError(t, json.Unmarshal(res.Content, &got))
	require.Equal(t, 1, got.ExitCode)
	require.Contains(t, got.Stderr, "ZeroDivisionError")
}

func TestInvokeAnnotatesTimeoutAndOOM(t *testing.T) {
	ex := &fakeExecutor{out: &sandbox.Output{Stdout: []byte("partial"), ExitCode: 137, TimedOut: true, StdoutTruncated: true}}
	res, err := invoke(t, New(ex, sandbox.DefaultLimits()), `{"code":"while True: pass","timeout_seconds":2}`)
	require.NoError(t, err)
	var got Result
	require.NoError(t, json.Unmarshal(res.Content, &got))
	require.True(t, got.TimedOut)
	require.True(t, got.StdoutTruncated)
	require.Equal(t, 137, got.ExitCode)
	require.Contains(t, got.Stderr, "killed after 2s wall-clock limit")

	ex.out = &sandbox.Output{Stderr: []byte("Killed"), ExitCode: 137, OOMKilled: true}
	res, err = invoke(t, New(ex, sandbox.DefaultLimits()), `{"code":"x"}`)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(res.Content, &got))
	require.True(t, got.OOMKilled)
	require.Equal(t, "Killed\n[sandbox] killed: exceeded 512 MB memory limit\n", got.Stderr)
}

func TestInvokeErrors(t *testing.T) {
	boom := errors.New("daemon down")
	_, err := invoke(t, New(&fakeExecutor{err: boom}, sandbox.DefaultLimits()), `{"code":"pass"}`)
	require.ErrorIs(t, err, boom, "an executor failure is a tool error (retryable tool_failed)")

	_, err = invoke(t, New(nil, sandbox.DefaultLimits()), `{"code":"pass"}`)
	require.ErrorIs(t, err, sandbox.ErrNoExecutor)

	ex := &fakeExecutor{out: &sandbox.Output{}}
	_, err = invoke(t, New(ex, sandbox.DefaultLimits()), `{"code":"   \n"}`)
	require.Error(t, err, "blank code")
	_, err = invoke(t, New(ex, sandbox.DefaultLimits()), `{"code":"pass","files":{"main.py":"x"}}`)
	require.Error(t, err, "files may not shadow main.py")
	_, err = invoke(t, New(ex, sandbox.DefaultLimits()), `{"code":"pass","files":{"../x":"x"}}`)
	require.Error(t, err, "path traversal")
}

func TestSchemaThroughRegistry(t *testing.T) {
	tool := New(&fakeExecutor{out: &sandbox.Output{}}, sandbox.Limits{MaxTimeout: 60 * time.Second})
	reg := tools.NewRegistry()
	require.NoError(t, reg.Register(tool))
	allow := []string{Name}

	ok := []string{
		`{"code":"print(1)"}`,
		`{"code":"print(1)","stdin":"x","files":{"data.csv":"a,b"},"timeout_seconds":60}`,
	}
	for _, args := range ok {
		_, err := reg.Resolve(Name, allow, json.RawMessage(args))
		require.NoError(t, err, args)
	}
	bad := map[string]string{
		"missing code":      `{}`,
		"empty code":        `{"code":""}`,
		"timeout too small": `{"code":"x","timeout_seconds":0}`,
		"timeout too large": `{"code":"x","timeout_seconds":61}`,
		"unknown field":     `{"code":"x","network":true}`,
		"file with slash":   `{"code":"x","files":{"a/b":"x"}}`,
		"file dotdot":       `{"code":"x","files":{"..":"x"}}`,
		"file not a string": `{"code":"x","files":{"a":1}}`,
	}
	for name, args := range bad {
		_, err := reg.Resolve(Name, allow, json.RawMessage(args))
		var verr *tools.ValidationError
		require.ErrorAs(t, err, &verr, name)
	}

	require.Equal(t, tools.Sandboxed, tool.TrustTier())
	require.Contains(t, tool.Description(), "up to 60 s")
}
