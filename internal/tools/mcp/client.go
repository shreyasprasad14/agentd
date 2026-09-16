package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shreyasprasad/agentd/internal/tools"
)

// clientName and clientVersion identify this runtime to servers in the MCP
// initialize handshake. Servers log it, and some gate behaviour on it.
const (
	clientName    = "agentd"
	clientVersion = "0.6.0"
)

const (
	// reconnectBase and reconnectMax bound the lazy reconnect. Exponential
	// rather than fixed because a server that is down is usually down for a
	// while, and a hot respawn loop on a stdio server is a fork bomb with
	// extra steps.
	reconnectBase = 250 * time.Millisecond
	reconnectMax  = 30 * time.Second

	// terminateGrace is how long a stdio server gets to exit after its stdin
	// is closed before it is signalled. The SDK's default is five seconds,
	// which a worker shutting down would pay once per server on top of
	// everything else it has to drain; two is enough for a server whose only
	// shutdown work is closing a file.
	terminateGrace = 2 * time.Second

	// pingTimeout bounds the liveness probe on the failure path. It is short
	// because its only job is to distinguish "the session is gone" from "that
	// one call was bad", and a slow answer to either is the same answer as no
	// answer for that purpose.
	pingTimeout = 2 * time.Second

	// maxStderrLine bounds one buffered line of a stdio server's stderr. The
	// child is not trusted to be terse.
	maxStderrLine = 4 << 10
)

// httpClient is shared by every http-transport server. It carries no Timeout
// on purpose: the per-call context bounds a tools/call, and a client-level
// timeout would also cut the long-lived streams the transport opens, which is
// a different thing to bound and not one this package has an opinion about.
var httpClient = &http.Client{}

// Client is one connected MCP server: the transport, the session, and the
// tool manifest discovered at boot.
//
// The manifest is read once, at Dial, and never re-read — not even across a
// reconnect. That is deliberate: the registry's names are fixed before any
// run is submitted, and the API validates a run's allowlist against them at
// submission time, so a server that comes back advertising a different tool
// set must not be able to change what a run already in flight was admitted
// against. The cost is that adding a tool to a server requires restarting
// agentd, which is the same cost as adding a builtin.
type Client struct {
	cfg  ServerConfig
	log  *slog.Logger
	sdk  *mcpsdk.Client
	list []tools.Tool

	// backoff bounds, fields rather than constants so tests do not have to
	// sleep for a quarter of a second to observe a reconnect.
	base, max time.Duration

	// mu guards everything below, and is held across a reconnect. Holding a
	// lock across a dial is usually wrong, but here it is the point: two
	// concurrent tool calls arriving at a dead stdio server must spawn one
	// subprocess between them, not two.
	mu      sync.Mutex
	sess    *mcpsdk.ClientSession
	cmd     *exec.Cmd
	closed  bool
	fails   int
	nextTry time.Time
}

// Dial connects to one server and discovers the tools it advertises. The
// context bounds the connect and the discovery, not the Client's lifetime: a
// connected Client outlives the context Dial was given.
func Dial(ctx context.Context, cfg ServerConfig, log *slog.Logger) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	c := &Client{
		cfg:  cfg,
		log:  log.With("mcp_server", cfg.Name),
		sdk:  mcpsdk.NewClient(&mcpsdk.Implementation{Name: clientName, Version: clientVersion}, nil),
		base: reconnectBase,
		max:  reconnectMax,
	}
	sess, cmd, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	list, err := c.discover(ctx, sess)
	if err != nil {
		// A server that connects but will not say what it can do is
		// unreachable for our purposes, so it is torn down here rather than
		// left half-alive holding a subprocess.
		_ = sess.Close()
		return nil, err
	}
	c.sess, c.cmd, c.list = sess, cmd, list
	return c, nil
}

// Name is the server's configured name, which is also the namespace its tools
// register under.
func (c *Client) Name() string { return c.cfg.Name }

// Tools returns the tools discovered at boot, already namespaced. The slice is
// not copied; do not modify it.
func (c *Client) Tools() []tools.Tool { return c.list }

// PID is the stdio subprocess's process id, or 0 for an http server or a
// disconnected one. It exists so a shutdown can be checked for orphans — the
// demo in docs/plans/m6.md shows what a leaked server looks like — and so the
// lifecycle test can assert the child is actually gone.
func (c *Client) PID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// Close shuts the server down. For a stdio server this closes the child's
// stdin, waits, and escalates to SIGTERM and SIGKILL, then reaps it — which
// is the whole reason Close does more than drop a reference. A worker that
// exited without it would leak one subprocess per server per restart.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	sess := c.sess
	c.sess, c.cmd = nil, nil
	c.mu.Unlock()

	if sess == nil {
		return nil
	}
	if err := sess.Close(); err != nil {
		return fmt.Errorf("close session: %w", err)
	}
	return nil
}

// call runs one tools/call, reconnecting first if the session was lost.
func (c *Client) call(ctx context.Context, tool string, args json.RawMessage) (*mcpsdk.CallToolResult, error) {
	sess, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	params := &mcpsdk.CallToolParams{Name: tool}
	if len(bytes.TrimSpace(args)) > 0 {
		// json.RawMessage marshals through as the exact bytes the registry
		// already validated, so the server sees what the schema approved
		// rather than a re-encoding of it.
		params.Arguments = args
	}
	res, err := sess.CallTool(ctx, params)
	if err != nil {
		c.callFailed(ctx, sess, err)
		return nil, err
	}
	c.succeeded()
	return res, nil
}

// session returns a live session, reconnecting if the last call killed one.
func (c *Client) session(ctx context.Context) (*mcpsdk.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.sess != nil {
		return c.sess, nil
	}
	if wait := time.Until(c.nextTry); wait > 0 {
		// Refusing here rather than dialing is what keeps a run that is
		// hammering a dead server from respawning it on every step. The model
		// sees a tool failure naming the wait, which is enough for it to try
		// something else.
		return nil, fmt.Errorf("%w: reconnect not attempted for another %s", ErrUnavailable, wait.Round(time.Millisecond))
	}
	sess, cmd, err := c.dial(ctx)
	if err != nil {
		c.fails++
		c.nextTry = time.Now().Add(c.backoff())
		c.log.Warn("mcp reconnect failed", "error", err, "attempt", c.fails, "retry_after", c.backoff())
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	c.sess, c.cmd = sess, cmd
	c.log.Info("mcp reconnected", "attempts", c.fails+1, "pid", pidOf(cmd))
	return sess, nil
}

// succeeded clears the backoff after a call that worked.
func (c *Client) succeeded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fails, c.nextTry = 0, time.Time{}
}

// callFailed decides whether a failed call means the session is gone.
//
// A tools/call error is not proof of that. A server can reject one malformed
// request with a protocol error and stay perfectly healthy, and tearing its
// subprocess down for that would respawn it — losing whatever it holds in
// memory — because of one bad call. So the session is probed with a short
// ping first, and only a session that fails the probe is torn down and put on
// the backoff clock. The alternative, reconnecting on every error, is simpler
// and wrong in exactly the case that matters: the model's first attempt at a
// tool's arguments.
func (c *Client) callFailed(ctx context.Context, sess *mcpsdk.ClientSession, cause error) {
	// WithoutCancel because the call's own context is usually already dead
	// when we get here — that is often why the call failed — and a probe that
	// inherits the dead deadline answers "gone" for every timeout.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pingTimeout)
	defer cancel()
	if err := sess.Ping(probeCtx, nil); err == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.sess != sess {
		// Another goroutine already replaced or closed it; do not restart its
		// backoff clock on the strength of an error against the old session.
		return
	}
	c.sess, c.cmd = nil, nil
	c.fails++
	c.nextTry = time.Now().Add(c.backoff())
	c.log.Warn("mcp session lost; will reconnect on the next call",
		"error", cause, "attempt", c.fails, "retry_after", c.backoff())
	// Closing reaps the subprocess of a server that died mid-call. The error
	// is dropped on purpose: the session is already gone, and the reason the
	// caller cares about is cause, not the corpse's exit status.
	_ = sess.Close()
}

// backoff is the wait before the next reconnect attempt. Caller holds mu.
func (c *Client) backoff() time.Duration {
	d := c.base
	for i := 1; i < c.fails && d < c.max; i++ {
		d *= 2
	}
	if d > c.max {
		d = c.max
	}
	return d
}

// dial builds the transport and connects. It returns the *exec.Cmd for a
// stdio server so Close and PID can reason about the child; http returns nil.
func (c *Client) dial(ctx context.Context) (*mcpsdk.ClientSession, *exec.Cmd, error) {
	transport, cmd, err := c.transport()
	if err != nil {
		return nil, nil, err
	}
	sess, err := c.sdk.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s server %q: %w", c.cfg.Transport, c.cfg.Name, err)
	}
	return sess, cmd, nil
}

func (c *Client) transport() (mcpsdk.Transport, *exec.Cmd, error) {
	switch c.cfg.Transport {
	case TransportStdio:
		cmd := exec.Command(c.cfg.Command, c.cfg.Args...)
		cmd.Env = append(os.Environ(), c.cfg.envPairs()...)
		// Without this the child's stderr goes to a pipe nobody reads, and a
		// server that dies during startup shows up here as "connect: EOF"
		// with its actual complaint discarded.
		cmd.Stderr = &stderrLog{log: c.log}
		return &mcpsdk.CommandTransport{Command: cmd, TerminateDuration: terminateGrace}, cmd, nil
	case TransportHTTP:
		return &mcpsdk.StreamableClientTransport{
			Endpoint:   c.cfg.URL,
			HTTPClient: httpClient,
			// This package registers no notification handlers, so the
			// standalone SSE stream would be a persistent connection per
			// server carrying messages nothing reads.
			DisableStandaloneSSE: true,
			MaxEventSize:         MaxEventBytes,
		}, nil, nil
	default:
		return nil, nil, fmt.Errorf("unknown transport %q", c.cfg.Transport)
	}
}

// discover reads the server's manifest and turns it into tools. A tool that
// cannot be adapted is skipped with a warning rather than failing the server:
// one unusable tool must not cost an operator the other five.
func (c *Client) discover(ctx context.Context, sess *mcpsdk.ClientSession) ([]tools.Tool, error) {
	var out []tools.Tool
	// The iterator handles cursor pagination, which a server with many tools
	// will use and which a hand-rolled single ListTools call would silently
	// truncate.
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list tools on %q: %w", c.cfg.Name, err)
		}
		if len(out) >= MaxTools {
			c.log.Warn("mcp server advertises more tools than the cap; the rest are ignored",
				"cap", MaxTools, "first_ignored", t.Name)
			break
		}
		tool, err := newTool(c, t)
		if err != nil {
			c.log.Warn("skipping mcp tool", "tool", t.Name, "error", err)
			continue
		}
		out = append(out, tool)
	}
	return out, nil
}

func pidOf(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

// stderrLog forwards a stdio server's stderr into the worker's log, one
// record per line, so a server's own diagnostics land in the same place as
// everything else rather than on the worker's terminal. It is written to from
// the os/exec copier goroutine only, which is why it needs no lock.
type stderrLog struct {
	log *slog.Logger
	buf []byte
}

func (w *stderrLog) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxStderrLine {
		// A server that never emits a newline would otherwise grow this
		// buffer without bound. Flush what is there and keep going.
		w.emit(w.buf)
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

func (w *stderrLog) emit(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	if len(line) > maxStderrLine {
		line = line[:maxStderrLine]
	}
	// Info rather than Warn: most servers log ordinary startup chatter here,
	// and a client that painted all of it as a problem would teach operators
	// to ignore the one line that is.
	w.log.Info("mcp server stderr", "line", string(line))
}
