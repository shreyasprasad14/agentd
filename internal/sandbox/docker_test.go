package sandbox_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/shreyasprasad/agentd/internal/sandbox"
	"github.com/shreyasprasad/agentd/internal/telemetry"
)

const testImage = "agentd/sandbox:python"

var (
	buildOnce sync.Once
	buildErr  error
	buildSkip string
)

// newExecutor returns an untraced Docker executor against the real daemon
// with the sandbox image built from deploy/sandbox. It skips under -short and
// when the docker CLI is not installed; anything else that fails is a failure.
func newExecutor(t *testing.T, limits sandbox.Limits) *sandbox.Docker {
	t.Helper()
	return newTracedExecutor(t, limits, nil)
}

// newRecordingExecutor is newExecutor with a tracer whose spans are kept in
// memory, so a test can assert on the bar the waterfall will draw without a
// collector anywhere.
func newRecordingExecutor(t *testing.T, limits sandbox.Limits) (*sandbox.Docker, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	return newTracedExecutor(t, limits, tp.Tracer("sandbox_test")), sr
}

// newTracedExecutor builds the executor both constructors above want; a nil
// tracer is the production default, where the executor falls back to no-op.
func newTracedExecutor(t *testing.T, limits sandbox.Limits, tracer trace.Tracer) *sandbox.Docker {
	t.Helper()
	return newInstrumentedExecutor(t, limits, tracer, nil)
}

// newInstrumentedExecutor is newTracedExecutor with metrics as well. Nil for
// either is the production default when the collector or the metrics listener
// is off, and the executor falls back to instruments that record nothing.
func newInstrumentedExecutor(t *testing.T, limits sandbox.Limits, tracer trace.Tracer, metrics *telemetry.Metrics) *sandbox.Docker {
	t.Helper()
	if testing.Short() {
		t.Skip("sandbox tests need Docker")
	}
	buildOnce.Do(func() {
		if _, err := exec.LookPath("docker"); err != nil {
			buildSkip = "docker CLI not on PATH"
			return
		}
		dir := filepath.Join("..", "..", "deploy", "sandbox")
		cmd := exec.Command("docker", "build", "-q", "-t", testImage, dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			t.Logf("docker build output:\n%s", out)
		}
	})
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	require.NoError(t, buildErr, "build sandbox image")

	d, err := sandbox.NewDocker(sandbox.DockerConfig{Image: testImage, Limits: limits, Tracer: tracer, Metrics: metrics})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.Ping(context.Background()))
	return d
}

// python builds a Spec that runs code as /work/in/main.py with a label that
// identifies this test, so assertNoContainers can find leftovers.
func python(t *testing.T, code string) sandbox.Spec {
	return sandbox.Spec{
		Cmd:    []string{"python3", sandbox.InputDir + "/main.py"},
		Files:  map[string][]byte{"main.py": []byte(code)},
		Labels: map[string]string{sandbox.LabelRunID: testRunID(t), sandbox.LabelTool: "python"},
	}
}

func testRunID(t *testing.T) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.Name())).String()
}

// assertNoContainers checks nothing labelled with this test's run id is
// left on the daemon, running or stopped.
func assertNoContainers(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label="+sandbox.LabelRunID+"="+testRunID(t)).Output()
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(string(out)), "sandbox container left behind")
}

func TestDockerRunCapturesStreamsAndExitCode(t *testing.T) {
	d := newExecutor(t, sandbox.Limits{})
	out, err := d.Run(context.Background(), python(t, `
import sys
print("to stdout")
print("to stderr", file=sys.stderr)
sys.exit(3)
`))
	require.NoError(t, err)
	require.Equal(t, "to stdout\n", string(out.Stdout))
	require.Equal(t, "to stderr\n", string(out.Stderr))
	require.Equal(t, 3, out.ExitCode)
	require.False(t, out.TimedOut)
	require.False(t, out.OOMKilled)
	require.False(t, out.StdoutTruncated)
	require.Greater(t, out.Duration, time.Duration(0))
	assertNoContainers(t)
}

func TestDockerRunStdinAndFiles(t *testing.T) {
	d := newExecutor(t, sandbox.Limits{})
	spec := python(t, `
import sys, os
print(sys.stdin.read().upper(), end="")
print(open("/work/in/data.txt").read())
print(sorted(os.listdir("/work/in")))
`)
	spec.Stdin = []byte("shout this")
	spec.Files["data.txt"] = []byte("from a file")
	out, err := d.Run(context.Background(), spec)
	require.NoError(t, err)
	require.Equal(t, 0, out.ExitCode, "stderr: %s", out.Stderr)
	require.Equal(t, "SHOUT THISfrom a file\n['data.txt', 'main.py']\n", string(out.Stdout))
	assertNoContainers(t)
}

func TestDockerRunTruncatesOutput(t *testing.T) {
	limits := sandbox.Limits{MaxOutputBytes: 1024}
	d := newExecutor(t, limits)
	out, err := d.Run(context.Background(), python(t, `
import sys
sys.stdout.write("x" * (1 << 20))
sys.stderr.write("short")
`))
	require.NoError(t, err)
	require.Equal(t, 0, out.ExitCode)
	require.Len(t, out.Stdout, 1024)
	require.True(t, out.StdoutTruncated)
	require.Equal(t, "short", string(out.Stderr))
	require.False(t, out.StderrTruncated)
	assertNoContainers(t)
}

func TestDockerRunTimeoutKills(t *testing.T) {
	d := newExecutor(t, sandbox.Limits{})
	spec := python(t, `
import time, sys
print("started", flush=True)
time.sleep(60)
print("never")
`)
	spec.Timeout = 2 * time.Second
	begin := time.Now()
	out, err := d.Run(context.Background(), spec)
	elapsed := time.Since(begin)
	require.NoError(t, err)
	require.True(t, out.TimedOut, "TimedOut")
	require.Equal(t, 137, out.ExitCode, "SIGKILL exit code")
	require.Equal(t, "started\n", string(out.Stdout), "output before the kill is kept")
	require.Less(t, elapsed, 15*time.Second, "kill must not wait for the sleep")
	assertNoContainers(t)
}

func TestDockerRunTimeoutIsClamped(t *testing.T) {
	d := newExecutor(t, sandbox.Limits{MaxTimeout: 2 * time.Second, DefaultTimeout: time.Second})
	spec := python(t, `import time; time.sleep(60)`)
	spec.Timeout = time.Hour
	begin := time.Now()
	out, err := d.Run(context.Background(), spec)
	require.NoError(t, err)
	require.True(t, out.TimedOut)
	require.Less(t, time.Since(begin), 15*time.Second)
	assertNoContainers(t)
}

func TestDockerRunCancelRemovesContainer(t *testing.T) {
	d := newExecutor(t, sandbox.Limits{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1500 * time.Millisecond)
		cancel()
	}()
	_, err := d.Run(ctx, python(t, `import time; time.sleep(60)`))
	require.ErrorIs(t, err, context.Canceled)
	assertNoContainers(t)
}

func TestDockerRunRejectsBadSpecs(t *testing.T) {
	d := newExecutor(t, sandbox.Limits{})
	_, err := d.Run(context.Background(), sandbox.Spec{})
	require.Error(t, err, "no command")
	_, err = d.Run(context.Background(), sandbox.Spec{Cmd: []string{"true"}, Files: map[string][]byte{"../escape": nil}})
	require.Error(t, err, "path traversal in file name")
	assertNoContainers(t)
}

func TestDockerSweepOrphans(t *testing.T) {
	d := newExecutor(t, sandbox.Limits{})
	runID := testRunID(t)

	// A leftover sandbox container, as a kill -9'd worker would leave it.
	orphan, err := exec.Command("docker", "create",
		"--label", sandbox.LabelSandbox+"=true", "--label", sandbox.LabelRunID+"="+runID,
		testImage, "python3", "-c", "pass").Output()
	require.NoError(t, err)
	orphanID := strings.TrimSpace(string(orphan))
	// A container that is not ours must be left alone.
	bystander, err := exec.Command("docker", "create", "--label", "agentd.test="+runID, testImage, "python3", "-c", "pass").Output()
	require.NoError(t, err)
	bystanderID := strings.TrimSpace(string(bystander))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", bystanderID, orphanID).Run() })

	// Too young to be an orphan: untouched.
	n, err := d.SweepOrphans(context.Background(), time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	n, err = d.SweepOrphans(context.Background(), 0)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1)
	assertNoContainers(t)

	out, err := exec.Command("docker", "ps", "-aq", "--filter", "id="+bystanderID).Output()
	require.NoError(t, err)
	require.NotEmpty(t, strings.TrimSpace(string(out)), "unlabelled container must survive the sweep")
}

// sandboxSpan returns the one sandbox.exec span recorded, with its attributes
// flattened for lookup. It selects by name rather than taking the only span
// because the Docker client instruments its own HTTP calls off the tracer
// provider it finds on the context, so each request to the daemon lands in
// the recorder as a child of ours — which is the daemon latency being visible
// in the waterfall, and is welcome.
func sandboxSpan(t *testing.T, sr *tracetest.SpanRecorder) (sdktrace.ReadOnlySpan, map[attribute.Key]attribute.Value) {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == telemetry.SpanSandbox {
			require.Nil(t, found, "one Run draws one sandbox.exec span")
			found = s
		}
	}
	require.NotNil(t, found, "no sandbox.exec span recorded")
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range found.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	return found, attrs
}

func TestDockerRunSpanRecordsExitCode(t *testing.T) {
	d, sr := newRecordingExecutor(t, sandbox.Limits{})
	_, err := d.Run(context.Background(), python(t, `import sys; sys.exit(3)`))
	require.NoError(t, err)

	span, attrs := sandboxSpan(t, sr)
	require.Equal(t, telemetry.SpanSandbox, span.Name())
	require.Equal(t, testImage, attrs[telemetry.AttrSandboxImage].AsString())
	require.Equal(t, int64(3), attrs[telemetry.AttrSandboxExitCode].AsInt64())
	require.False(t, attrs[telemetry.AttrSandboxTimedOut].AsBool())
	require.False(t, attrs[telemetry.AttrSandboxOOM].AsBool())
	// ADR-14: the script failed, the sandbox did not. A red bar here would
	// mean the platform is broken, and it is not.
	require.Equal(t, codes.Unset, span.Status().Code, "a nonzero exit is a fact, not a failed span")
	assertNoContainers(t)
}

func TestDockerRunSpanRecordsTimeout(t *testing.T) {
	d, sr := newRecordingExecutor(t, sandbox.Limits{})
	spec := python(t, `import time; time.sleep(60)`)
	spec.Timeout = 2 * time.Second
	_, err := d.Run(context.Background(), spec)
	require.NoError(t, err)

	span, attrs := sandboxSpan(t, sr)
	require.True(t, attrs[telemetry.AttrSandboxTimedOut].AsBool())
	require.Equal(t, int64(137), attrs[telemetry.AttrSandboxExitCode].AsInt64())
	require.Equal(t, codes.Unset, span.Status().Code, "a timeout is a fact about the script too")
	assertNoContainers(t)
}

func TestDockerRunSpanRecordsSandboxFailure(t *testing.T) {
	newExecutor(t, sandbox.Limits{}) // the daemon is up; the image is not the point here
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	d, err := sandbox.NewDocker(sandbox.DockerConfig{
		Image:  "agentd/does-not-exist:" + uuid.NewString()[:8],
		Tracer: tp.Tracer("sandbox_test"),
	})
	require.NoError(t, err)
	defer d.Close()

	_, err = d.Run(context.Background(), python(t, `pass`))
	require.Error(t, err, "a missing image is the sandbox failing, not the script")

	span, _ := sandboxSpan(t, sr)
	require.Equal(t, codes.Error, span.Status().Code)
	require.NotEmpty(t, span.Events(), "the error is recorded on the span")
}

func TestDockerPingReportsMissingImage(t *testing.T) {
	newExecutor(t, sandbox.Limits{}) // ensures the daemon is there
	d, err := sandbox.NewDocker(sandbox.DockerConfig{Image: "agentd/does-not-exist:" + uuid.NewString()[:8]})
	require.NoError(t, err)
	defer d.Close()
	err = d.Ping(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "make sandbox-build")
}

// TestDockerRunCountsTimeouts is the sandbox's half of M4's counters. A
// timeout is the one container outcome with a counter of its own: a nonzero
// exit is the model's Python being wrong (ADR-14), while a rising timeout rate
// means -sandbox-timeout is wrong for the work.
func TestDockerRunCountsTimeouts(t *testing.T) {
	m := telemetry.NewMetrics()
	d := newInstrumentedExecutor(t, sandbox.Limits{}, nil, m)

	spec := python(t, `import time; time.sleep(60)`)
	spec.Timeout = 2 * time.Second
	out, err := d.Run(context.Background(), spec)
	require.NoError(t, err)
	require.True(t, out.TimedOut)

	require.Equal(t, 1.0, counterValue(t, m, "agentd_sandbox_timeouts_total"))

	// A script that merely failed is not a timeout, and must not move it.
	_, err = d.Run(context.Background(), python(t, `import sys; sys.exit(3)`))
	require.NoError(t, err)
	require.Equal(t, 1.0, counterValue(t, m, "agentd_sandbox_timeouts_total"))
	assertNoContainers(t)
}

// counterValue reads one unlabelled counter back the way a scrape would. The
// instruments themselves are unexported on purpose — call sites go through
// the nil-safe methods — so the exposition is the seam a test gets.
func counterValue(t *testing.T, m *telemetry.Metrics, name string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() == name {
			require.Len(t, f.GetMetric(), 1)
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("no counter %q in the registry", name)
	return 0
}
