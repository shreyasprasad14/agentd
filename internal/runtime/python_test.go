package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/sandbox"
	"github.com/shreyasprasad/agentd/internal/tools/builtin"
	"github.com/shreyasprasad/agentd/internal/tools/python"
)

// scriptedExecutor stands in for Docker: it hands back queued outputs and
// records the specs it was asked to run.
type scriptedExecutor struct {
	mu    sync.Mutex
	outs  []*sandbox.Output
	specs []sandbox.Spec
}

func (s *scriptedExecutor) Run(_ context.Context, spec sandbox.Spec) (*sandbox.Output, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs = append(s.specs, spec)
	if len(s.outs) == 0 {
		return &sandbox.Output{}, nil
	}
	out := s.outs[0]
	s.outs = s.outs[1:]
	return out, nil
}

func pythonCall(id, code string) *model.Response {
	return fake.ToolUse(id, python.Name, map[string]any{"code": code}, usage)
}

// TestLoopRunsPythonThroughTheSandbox drives the python tool through the
// real loop with a scripted executor: the sandbox's exit code lands in
// tool_succeeded.exit_code, and the script's output reaches the next model
// call inside the data envelope. A failing script is still a succeeded
// tool call, with the failure visible to the model as data.
func TestLoopRunsPythonThroughTheSandbox(t *testing.T) {
	exec := &scriptedExecutor{outs: []*sandbox.Output{
		{Stdout: []byte("2026-10-15\n"), ExitCode: 0, Duration: 300 * time.Millisecond},
		{Stderr: []byte("Traceback (most recent call last):\nNameError: name 'x' is not defined\n"), ExitCode: 1, Duration: 200 * time.Millisecond},
	}}
	f := newFixture(t, fake.New(
		pythonCall("t1", "import datetime; print(datetime.date(2026,9,3) + datetime.timedelta(days=42))"),
		pythonCall("t2", "print(x)"),
		finishCall("t3", "The deadline is 2026-10-15."),
	))
	f.registry.MustRegister(python.New(exec, sandbox.DefaultLimits()))
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("compute the deadline with python", runOpts{tools: []string{python.Name, builtin.FinishName}})
	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	evs := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolSucceeded,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolSucceeded,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolSucceeded,
		runtime.EventRunFinished,
	}, eventTypes(evs))

	first := decode[runtime.ToolSucceededPayload](t, evs[4])
	require.Equal(t, python.Name, first.Name)
	require.Equal(t, 0, first.ExitCode)
	var r1 python.Result
	require.NoError(t, json.Unmarshal(first.Result, &r1))
	require.Equal(t, "2026-10-15\n", r1.Stdout)
	require.Equal(t, int64(300), r1.DurationMS)

	second := decode[runtime.ToolSucceededPayload](t, evs[8])
	require.Equal(t, 1, second.ExitCode, "a failing script is a result with its exit code, not a tool_failed")
	var r2 python.Result
	require.NoError(t, json.Unmarshal(second.Result, &r2))
	require.Contains(t, r2.Stderr, "NameError")

	// The executor was handed the run's identity for container labels and
	// the code as /work/in/main.py.
	require.Len(t, exec.specs, 2)
	require.Equal(t, id.String(), exec.specs[0].Labels[sandbox.LabelRunID])
	require.Equal(t, "4", exec.specs[0].Labels[sandbox.LabelSeq])
	require.Contains(t, string(exec.specs[0].Files[python.ScriptName]), "timedelta(days=42)")
	require.Equal(t, sandbox.DefaultLimits().DefaultTimeout, exec.specs[0].Timeout)

	// The model saw stdout, then the traceback, as enveloped data.
	reqs := f.provider.Requests()
	require.Len(t, reqs, 3)
	res1 := reqs[1].Messages[2].Content[0]
	require.Equal(t, model.BlockToolResult, res1.Type)
	require.False(t, res1.IsError)
	require.True(t, strings.HasPrefix(res1.Content, `<tool_result tool="run_python" seq=4>`), res1.Content)
	require.Contains(t, res1.Content, `"stdout": "2026-10-15\n"`)
	res2 := reqs[2].Messages[4].Content[0]
	require.False(t, res2.IsError, "nonzero exit is not is_error; the model reads exit_code")
	require.Contains(t, res2.Content, `"exit_code": 1`)
	require.Contains(t, res2.Content, "NameError")
}
