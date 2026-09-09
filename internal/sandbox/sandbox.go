// Package sandbox runs untrusted code in a fresh, isolated container per
// call. The Executor interface is what tools depend on; Docker is the one
// implementation (spec §7). Resource limits, timeouts, and output truncation
// are the executor's job, so a tool built on it cannot forget them.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// InputDir is where Spec.Files land inside the container. It is created in
// the image and is read-only at runtime like the rest of the rootfs.
const InputDir = "/work/in"

// Limits are the resource caps applied to every container. The defaults are
// the flag set from spec §7.
type Limits struct {
	// MemoryBytes caps RSS; swap is disabled (memory-swap == memory).
	MemoryBytes int64
	// CPUs is the CFS quota in whole or fractional CPUs.
	CPUs float64
	// PidsLimit bounds the process count, which is what contains a fork bomb.
	PidsLimit int64
	// TmpfsSize is the size of the writable /tmp, in Docker's size syntax.
	TmpfsSize string
	// MaxOutputBytes is kept per stream (stdout, stderr). Anything past it is
	// counted and dropped so the context window and the worker's memory are
	// both bounded.
	MaxOutputBytes int
	// DefaultTimeout applies when a Spec names none; MaxTimeout clamps what a
	// Spec may ask for.
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

// DefaultLimits is spec §7: 512 MiB, 1 CPU, 128 pids, 64 MiB /tmp, 16 KiB
// per output stream, 30 s default and 120 s maximum wall clock.
func DefaultLimits() Limits {
	return Limits{
		MemoryBytes:    512 << 20,
		CPUs:           1,
		PidsLimit:      128,
		TmpfsSize:      "64m",
		MaxOutputBytes: 16 << 10,
		DefaultTimeout: 30 * time.Second,
		MaxTimeout:     120 * time.Second,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MemoryBytes <= 0 {
		l.MemoryBytes = d.MemoryBytes
	}
	if l.CPUs <= 0 {
		l.CPUs = d.CPUs
	}
	if l.PidsLimit <= 0 {
		l.PidsLimit = d.PidsLimit
	}
	if l.TmpfsSize == "" {
		l.TmpfsSize = d.TmpfsSize
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = d.MaxOutputBytes
	}
	if l.DefaultTimeout <= 0 {
		l.DefaultTimeout = d.DefaultTimeout
	}
	if l.MaxTimeout <= 0 {
		l.MaxTimeout = d.MaxTimeout
	}
	if l.DefaultTimeout > l.MaxTimeout {
		l.DefaultTimeout = l.MaxTimeout
	}
	return l
}

// Timeout resolves a requested timeout against the limits: zero takes the
// default, anything above the maximum is clamped to it.
func (l Limits) Timeout(requested time.Duration) time.Duration {
	if requested <= 0 {
		return l.DefaultTimeout
	}
	if requested > l.MaxTimeout {
		return l.MaxTimeout
	}
	return requested
}

// Spec describes one sandboxed execution.
type Spec struct {
	// Cmd is the argv to run. Required.
	Cmd []string
	// Stdin is written to the process and then closed.
	Stdin []byte
	// Files are placed under InputDir before the process starts. Keys are
	// plain file names: no directories, no "..", no leading slash.
	Files map[string][]byte
	// Timeout is the wall-clock budget; see Limits.Timeout.
	Timeout time.Duration
	// Labels are attached to the container so `docker ps` and the orphan
	// sweep can tell whose it is. Tools set run_id, seq, and their name.
	Labels map[string]string
}

// Output is what a sandboxed process produced. A nonzero exit code, a
// timeout, or an OOM kill are all reported here rather than as an error:
// they are facts about the script the model needs to see, not failures of
// the sandbox.
type Output struct {
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	ExitCode        int
	TimedOut        bool
	OOMKilled       bool
	Duration        time.Duration
}

// Executor runs a Spec to completion in an isolated environment. Run returns
// an error only when the sandbox itself could not do its job (daemon down,
// image missing, container refused); ctx cancellation is returned as
// ctx.Err() after the container has been killed and removed.
type Executor interface {
	Run(ctx context.Context, spec Spec) (*Output, error)
}

// ErrNoExecutor is returned by tools registered without a sandbox, which is
// how the API server lists them without being able to run them.
var ErrNoExecutor = errors.New("sandbox: no executor configured on this process")

// Label keys every sandbox container carries.
const (
	LabelSandbox = "agentd.sandbox"
	LabelRunID   = "agentd.run_id"
	LabelSeq     = "agentd.seq"
	LabelTool    = "agentd.tool"
)

// ValidateFileName accepts a plain file name for Spec.Files.
func ValidateFileName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return fmt.Errorf("file name %q is empty or reserved", name)
	case strings.ContainsAny(name, "/\\\x00"):
		return fmt.Errorf("file name %q must not contain path separators", name)
	case len(name) > 255:
		return fmt.Errorf("file name %q is too long", name)
	}
	return nil
}

func (s Spec) validate() error {
	if len(s.Cmd) == 0 {
		return errors.New("sandbox: spec has no command")
	}
	for name := range s.Files {
		if err := ValidateFileName(name); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
	}
	return nil
}
