// Package python is the sandboxed script tool (spec §8). Every call runs in
// a fresh container through a sandbox.Executor; the tool itself only shapes
// the spec and the result.
package python

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shreyasprasad/agentd/internal/sandbox"
	"github.com/shreyasprasad/agentd/internal/tools"
)

// Name is the tool's name. The spec calls it `python`; it is `run_python`
// because Ollama (0.33+) reserves `python` for its own builtin tool and
// silently drops any tool call by that name, whatever model is serving
// (verified: 0 of 12 calls parsed as `python`, 5 of 6 as `run_python`, same
// request otherwise). See docs/plans/m2.md.
const Name = "run_python"

// ScriptName is where the code lands under sandbox.InputDir.
const ScriptName = "main.py"

const (
	maxCodeBytes  = 64 << 10
	maxStdinBytes = 64 << 10
	maxFiles      = 16
	maxFileBytes  = 256 << 10
)

// Tool runs Python in the sandbox. A nil executor makes a tool that can be
// listed and allowlisted but not invoked, which is what the API server
// needs.
type Tool struct {
	exec   sandbox.Executor
	limits sandbox.Limits
}

// New builds the tool. limits should be the executor's effective limits so
// the schema and description advertise the real bounds.
func New(exec sandbox.Executor, limits sandbox.Limits) *Tool {
	if limits.MaxTimeout <= 0 || limits.DefaultTimeout <= 0 || limits.MaxOutputBytes <= 0 {
		d := sandbox.DefaultLimits()
		if limits.MaxTimeout <= 0 {
			limits.MaxTimeout = d.MaxTimeout
		}
		if limits.DefaultTimeout <= 0 {
			limits.DefaultTimeout = d.DefaultTimeout
		}
		if limits.MaxOutputBytes <= 0 {
			limits.MaxOutputBytes = d.MaxOutputBytes
		}
		if limits.MemoryBytes <= 0 {
			limits.MemoryBytes = d.MemoryBytes
		}
		if limits.CPUs <= 0 {
			limits.CPUs = d.CPUs
		}
	}
	return &Tool{exec: exec, limits: limits}
}

// Args is the tool input.
type Args struct {
	Code           string            `json:"code"`
	Stdin          string            `json:"stdin,omitempty"`
	Files          map[string]string `json:"files,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

// Result is what the model gets back. Everything the sandbox observed is
// here as data: a failing script is a result to reason about, not an error.
type Result struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out"`
	OOMKilled       bool   `json:"oom_killed"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	DurationMS      int64  `json:"duration_ms"`
}

func (t *Tool) Name() string { return Name }

func (t *Tool) Description() string {
	return fmt.Sprintf("Run a Python 3.12 script in an isolated sandbox and return its stdout, stderr, and exit code. "+
		"Standard library only (no pip, no network access). Limits: %d MB memory, %g CPU, %d s wall clock by default "+
		"(up to %d s via `timeout_seconds`). The filesystem is read-only except /tmp. Extra input files given in `files` "+
		"appear under %s/. Print the values you need; stdout and stderr are each truncated to %d KB.",
		t.limits.MemoryBytes>>20, t.limits.CPUs, int(t.limits.DefaultTimeout.Seconds()), int(t.limits.MaxTimeout.Seconds()),
		sandbox.InputDir, t.limits.MaxOutputBytes>>10)
}

func (t *Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "code":            {"type": "string", "minLength": 1, "maxLength": ` + strconv.Itoa(maxCodeBytes) + `, "description": "Python source to run as ` + sandbox.InputDir + `/` + ScriptName + `."},
    "stdin":           {"type": "string", "maxLength": ` + strconv.Itoa(maxStdinBytes) + `, "description": "Text fed to the script's standard input."},
    "files":           {"type": "object", "maxProperties": ` + strconv.Itoa(maxFiles) + `,
                        "propertyNames": {"pattern": "^[^/\\\\]{1,255}$", "not": {"enum": [".", ".."]}},
                        "additionalProperties": {"type": "string", "maxLength": ` + strconv.Itoa(maxFileBytes) + `},
                        "description": "Extra input files by name, placed read-only under ` + sandbox.InputDir + `/."},
    "timeout_seconds": {"type": "integer", "minimum": 1, "maximum": ` + strconv.Itoa(int(t.limits.MaxTimeout.Seconds())) + `, "description": "Wall-clock limit for this call."}
  },
  "required": ["code"],
  "additionalProperties": false
}`)
}

func (t *Tool) TrustTier() tools.TrustTier { return tools.Sandboxed }

// Invoke runs the script. The registry has already validated Args against
// the schema; the checks here are the ones a schema cannot express.
func (t *Tool) Invoke(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	if t.exec == nil {
		return tools.Result{}, sandbox.ErrNoExecutor
	}
	var args Args
	if err := json.Unmarshal(inv.Args, &args); err != nil {
		return tools.Result{}, err
	}
	if strings.TrimSpace(args.Code) == "" {
		return tools.Result{}, fmt.Errorf("code must not be empty")
	}
	if _, clash := args.Files[ScriptName]; clash {
		return tools.Result{}, fmt.Errorf("files must not include %s; that is where code goes", ScriptName)
	}

	spec, err := t.spec(args, inv)
	if err != nil {
		return tools.Result{}, err
	}
	out, err := t.exec.Run(ctx, spec)
	if err != nil {
		return tools.Result{}, err
	}

	res := Result{
		Stdout:          string(out.Stdout),
		Stderr:          string(out.Stderr),
		ExitCode:        out.ExitCode,
		TimedOut:        out.TimedOut,
		OOMKilled:       out.OOMKilled,
		StdoutTruncated: out.StdoutTruncated,
		StderrTruncated: out.StderrTruncated,
		DurationMS:      out.Duration.Milliseconds(),
	}
	if res.TimedOut {
		res.Stderr = appendNote(res.Stderr, fmt.Sprintf("[sandbox] killed after %s wall-clock limit", spec.Timeout))
	}
	if res.OOMKilled {
		res.Stderr = appendNote(res.Stderr, fmt.Sprintf("[sandbox] killed: exceeded %d MB memory limit", t.limits.MemoryBytes>>20))
	}
	content, err := json.Marshal(res)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: content, ExitCode: out.ExitCode}, nil
}

// spec turns validated args into a sandbox.Spec.
func (t *Tool) spec(args Args, inv tools.Invocation) (sandbox.Spec, error) {
	files := map[string][]byte{ScriptName: []byte(args.Code)}
	names := make([]string, 0, len(args.Files))
	for name := range args.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := sandbox.ValidateFileName(name); err != nil {
			return sandbox.Spec{}, err
		}
		files[name] = []byte(args.Files[name])
	}
	return sandbox.Spec{
		Cmd:     []string{"python3", sandbox.InputDir + "/" + ScriptName},
		Stdin:   []byte(args.Stdin),
		Files:   files,
		Timeout: t.limits.Timeout(time.Duration(args.TimeoutSeconds) * time.Second),
		Labels: map[string]string{
			sandbox.LabelRunID: inv.RunID.String(),
			sandbox.LabelSeq:   strconv.Itoa(int(inv.Seq)),
			sandbox.LabelTool:  Name,
		},
	}, nil
}

func appendNote(stderr, note string) string {
	if stderr != "" && !strings.HasSuffix(stderr, "\n") {
		stderr += "\n"
	}
	return stderr + note + "\n"
}
