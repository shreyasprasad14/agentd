package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/shreyasprasad/agentd/internal/telemetry"
)

// sandboxUser is the unprivileged uid:gid every container runs as (nobody).
const sandboxUser = "65534:65534"

// DockerConfig configures the Docker executor.
type DockerConfig struct {
	// Image is the sandbox image, built from deploy/sandbox.
	Image string
	// Limits override DefaultLimits field by field; zero fields keep the default.
	Limits Limits
	// Host is the daemon address. Empty resolves it like the docker CLI:
	// DOCKER_HOST, then the current CLI context, then the default socket.
	Host string
	Log  *slog.Logger
	// Tracer draws one sandbox.exec span per Run, under whichever tool asked
	// for the container. Nil is fine and means no spans.
	Tracer trace.Tracer
	// Metrics counts timeouts and swept orphans. Nil is fine and records
	// nothing; every method on it is nil-safe.
	Metrics *telemetry.Metrics
}

// Docker runs each Spec in a fresh container through the Engine API. It
// talks to the daemon over the socket, so it works the same whether the
// worker runs on the host or in a container with the socket mounted.
type Docker struct {
	cli     *client.Client
	image   string
	limits  Limits
	log     *slog.Logger
	tracer  trace.Tracer
	metrics *telemetry.Metrics
}

// NewDocker builds the executor. It does not contact the daemon; call Ping
// to check the daemon and the image are reachable.
func NewDocker(cfg DockerConfig) (*Docker, error) {
	if cfg.Image == "" {
		return nil, errors.New("sandbox: image is required")
	}
	host := cfg.Host
	if host == "" {
		host = DockerHost()
	}
	cli, err := client.New(client.FromEnv, client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("sandbox: docker client: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	tracer := cfg.Tracer
	if tracer == nil {
		// Resolved once here so Run never asks whether tracing is on: a no-op
		// tracer hands out spans that cost nothing.
		tracer = noop.NewTracerProvider().Tracer("")
	}
	return &Docker{cli: cli, image: cfg.Image, limits: cfg.Limits.withDefaults(), log: log,
		tracer: tracer, metrics: cfg.Metrics}, nil
}

// Host is the daemon address in use.
func (d *Docker) Host() string { return d.cli.DaemonHost() }

// Limits returns the effective limits.
func (d *Docker) Limits() Limits { return d.limits }

// Image returns the sandbox image name.
func (d *Docker) Image() string { return d.image }

// Close releases the client's connections.
func (d *Docker) Close() error { return d.cli.Close() }

// Ping checks the daemon answers and the sandbox image is present.
func (d *Docker) Ping(ctx context.Context) error {
	if _, err := d.cli.Ping(ctx, client.PingOptions{}); err != nil {
		return fmt.Errorf("sandbox: docker daemon: %w", err)
	}
	if _, err := d.cli.ImageInspect(ctx, d.image); err != nil {
		return fmt.Errorf("sandbox: image %q: %w (build it with `make sandbox-build`)", d.image, err)
	}
	return nil
}

// Run implements Executor.
//
// The span it draws is deliberately not an error span when the script fails:
// a nonzero exit, a timeout, and an OOM kill are facts about the code the
// model wrote (ADR-14), so they are attributes on an OK span. Only a sandbox
// that could not do its job — daemon down, image missing, container refused —
// ends in error, which keeps "red bar in the waterfall" meaning "the platform
// is broken" rather than "the model's Python had a typo".
func (d *Docker) Run(ctx context.Context, spec Spec) (*Output, error) {
	// Validation is outside the span: a spec rejected before any container
	// exists has no container lifetime to draw, and the caller's tool.invoke
	// span already covers it.
	if err := spec.validate(); err != nil {
		return nil, err
	}
	ctx, span := d.tracer.Start(ctx, telemetry.SpanSandbox,
		trace.WithAttributes(telemetry.AttrSandboxImage.String(d.image)))
	out, err := d.run(ctx, spec)
	if out != nil {
		span.SetAttributes(
			telemetry.AttrSandboxExitCode.Int(out.ExitCode),
			telemetry.AttrSandboxTimedOut.Bool(out.TimedOut),
			telemetry.AttrSandboxOOM.Bool(out.OOMKilled),
		)
		if out.TimedOut {
			// A timeout is the one sandbox outcome worth its own counter:
			// it is not a bug in the model's code but a limit the platform
			// imposed, and a rising rate means -sandbox-timeout is wrong for
			// the work rather than that the scripts got worse.
			d.metrics.SandboxTimedOut()
		}
	}
	telemetry.End(span, err)
	return out, err
}

// run is Run's body, split out so the span can be ended in one place rather
// than at each of the half-dozen ways a container can refuse to run.
func (d *Docker) run(ctx context.Context, spec Spec) (*Output, error) {
	timeout := d.limits.Timeout(spec.Timeout)
	withStdin := len(spec.Stdin) > 0

	labels := map[string]string{LabelSandbox: "true"}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	pids := d.limits.PidsLimit

	cfg := &container.Config{
		Image:           d.image,
		Cmd:             spec.Cmd,
		User:            sandboxUser,
		WorkingDir:      "/tmp",
		Env:             []string{"HOME=/tmp"},
		AttachStdin:     withStdin,
		OpenStdin:       withStdin,
		StdinOnce:       withStdin,
		AttachStdout:    true,
		AttachStderr:    true,
		Labels:          labels,
		NetworkDisabled: true,
	}
	host := &container.HostConfig{
		// spec §7, flag for flag. See docs/plans/m2.md for the mapping.
		NetworkMode:    container.NetworkMode(network.NetworkNone),
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": "rw,exec,nosuid,size=" + d.limits.TmpfsSize},
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		// Output is captured over the attach stream below; never let the
		// daemon also spool it to disk, where a flood could fill the host.
		LogConfig: container.LogConfig{Type: "none"},
		Resources: container.Resources{
			Memory:     d.limits.MemoryBytes,
			MemorySwap: d.limits.MemoryBytes, // equal to Memory: no swap
			NanoCPUs:   int64(d.limits.CPUs * 1e9),
			PidsLimit:  &pids,
		},
	}
	if len(spec.Files) > 0 {
		// The daemon refuses to copy into a read-only rootfs, so inputs go
		// into an anonymous volume mounted at InputDir. The volume inherits
		// the image's root-owned 0555 directory, so uid 65534 cannot write
		// to it, and it is deleted with the container.
		host.Mounts = []mount.Mount{{Type: mount.TypeVolume, Target: InputDir}}
	}

	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: host})
	if err != nil {
		return nil, fmt.Errorf("sandbox: create container: %w", err)
	}
	id := created.ID
	log := d.log.With("container", id[:12])
	for _, w := range created.Warnings {
		log.Warn("docker create warning", "warning", w)
	}
	defer d.remove(id, log)

	if len(spec.Files) > 0 {
		_, err := d.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{
			DestinationPath: InputDir,
			Content:         tarFiles(spec.Files),
		})
		if err != nil {
			return nil, fmt.Errorf("sandbox: copy input files: %w", err)
		}
	}

	attach, err := d.cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{
		Stream: true, Stdin: withStdin, Stdout: true, Stderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: attach: %w", err)
	}
	defer attach.Close()

	stdout := newLimitWriter(d.limits.MaxOutputBytes)
	stderr := newLimitWriter(d.limits.MaxOutputBytes)
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		if _, err := stdcopy.StdCopy(stdout, stderr, attach.Reader); err != nil && !errors.Is(err, io.EOF) {
			log.Debug("attach stream ended", "error", err)
		}
	}()

	// Wait is registered before Start with next-exit so a process that
	// exits instantly cannot slip past it. The timeout context bounds the
	// whole execution from here.
	waitCtx, cancelWait := context.WithTimeout(ctx, timeout)
	defer cancelWait()
	wait := d.cli.ContainerWait(waitCtx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})

	started := time.Now()
	if _, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox: start container: %w", err)
	}
	if withStdin {
		go func() {
			if _, err := attach.Conn.Write(spec.Stdin); err != nil {
				log.Debug("stdin write ended early", "error", err)
			}
			_ = attach.CloseWrite()
		}()
	}

	out := &Output{}
	select {
	case res := <-wait.Result:
		out.ExitCode = int(res.StatusCode)
		if res.Error != nil && res.Error.Message != "" {
			return nil, fmt.Errorf("sandbox: container wait: %s", res.Error.Message)
		}
	case err := <-wait.Error:
		switch {
		case ctx.Err() != nil:
			// Worker shutting down: the deferred force-remove kills it. The
			// loop records nothing and the next worker re-executes.
			return nil, ctx.Err()
		case waitCtx.Err() != nil:
			out.TimedOut = true
			out.ExitCode = d.killAndWait(id, log)
		default:
			return nil, fmt.Errorf("sandbox: container wait: %w", err)
		}
	}
	out.Duration = time.Since(started)

	// The daemon closes the attach stream once the process is gone; give
	// the copier a moment to drain what is buffered.
	select {
	case <-copied:
	case <-time.After(5 * time.Second):
		log.Warn("attach stream did not close after exit; output may be incomplete")
	}
	out.Stdout, out.StdoutTruncated = stdout.Bytes(), stdout.Truncated()
	out.Stderr, out.StderrTruncated = stderr.Bytes(), stderr.Truncated()

	if insp, err := d.cli.ContainerInspect(context.WithoutCancel(ctx), id, client.ContainerInspectOptions{}); err == nil && insp.Container.State != nil {
		out.OOMKilled = insp.Container.State.OOMKilled
	}

	log.Info("sandbox run finished", "exit_code", out.ExitCode, "timed_out", out.TimedOut, "oom_killed", out.OOMKilled,
		"duration_ms", out.Duration.Milliseconds(), "stdout_bytes", stdout.Total(), "stderr_bytes", stderr.Total())
	return out, nil
}

// killAndWait sends SIGKILL and returns the resulting exit code (137 unless
// the process beat the signal). It uses its own context because the caller's
// may already be expired.
func (d *Docker) killAndWait(id string, log *slog.Logger) int {
	bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.cli.ContainerKill(bg, id, client.ContainerKillOptions{Signal: "KILL"}); err != nil && !isNotFound(err) {
		log.Warn("kill after timeout failed", "error", err)
	}
	w := d.cli.ContainerWait(bg, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-w.Result:
		return int(res.StatusCode)
	case <-w.Error:
		return 137
	}
}

// remove is the `--rm`. It runs on a background context so a cancelled
// caller still cleans up, and force-removes so a still-running (timed out,
// or cancelled) container dies with its anonymous volume.
func (d *Docker) remove(id string, log *slog.Logger) {
	bg, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := d.cli.ContainerRemove(bg, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !isNotFound(err) {
		log.Warn("remove sandbox container failed; the orphan sweep will retry", "error", err)
	}
}

// SweepOrphans force-removes sandbox containers created more than olderThan
// ago. A worker that dies mid-call leaves its container behind with nobody
// to enforce the timeout, so every worker sweeps at boot; containers younger
// than the maximum timeout may belong to a live sibling and are left alone.
func (d *Docker) SweepOrphans(ctx context.Context, olderThan time.Duration) (int, error) {
	res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelSandbox+"=true"),
	})
	if err != nil {
		return 0, fmt.Errorf("sandbox: list containers: %w", err)
	}
	cutoff := time.Now().Add(-olderThan).Unix()
	n := 0
	for _, c := range res.Items {
		if c.Created > cutoff {
			continue
		}
		_, err := d.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if err != nil && !isNotFound(err) {
			d.log.Warn("remove orphan sandbox failed", "container", c.ID[:12], "error", err)
			continue
		}
		d.log.Info("removed orphan sandbox container", "container", c.ID[:12],
			"run_id", c.Labels[LabelRunID], "seq", c.Labels[LabelSeq], "age", time.Since(time.Unix(c.Created, 0)).Round(time.Second))
		n++
	}
	// Counted rather than gauged: a sweep's finding is an event — a worker
	// died mid-call and left something behind — and the total is what says
	// how often that has happened since this process started.
	d.metrics.SandboxOrphansSwept(n)
	return n, nil
}

// OrphanAge is how old a sandbox container must be before SweepOrphans may
// assume no live worker owns it: the longest a run is allowed plus slack for
// create/remove latency.
func (d *Docker) OrphanAge() time.Duration { return d.limits.MaxTimeout + 30*time.Second }

// tarFiles packs Spec.Files as root-owned, read-only regular files.
func tarFiles(files map[string][]byte) io.Reader {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		_ = tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0o444,
			Size:     int64(len(files[name])),
			ModTime:  time.Unix(0, 0),
		})
		_, _ = tw.Write(files[name])
	}
	_ = tw.Close()
	return &buf
}

// isNotFound matches the daemon's 404s and the client's own not-found errors.
func isNotFound(err error) bool {
	var nf interface{ NotFound() }
	return errors.As(err, &nf)
}
