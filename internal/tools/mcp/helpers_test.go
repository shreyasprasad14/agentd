package mcp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/tools/mcp/testserver"
)

// TestMain lets this test binary double as the MCP server under test.
//
// The alternative was to `go build` the fixture binary from TestMain, which
// costs a toolchain invocation and a temp directory on every run and can fail
// for reasons that have nothing to do with the code being tested. Re-execing
// ourselves is the os/exec package's own trick, and it makes the spawned
// server exactly as real: a separate process, a real pipe, a real protocol.
func TestMain(m *testing.M) {
	if opts, ok := testserver.OptionsFromEnv(); ok {
		if err := testserver.Run(context.Background(), opts); err != nil {
			fmt.Fprintln(os.Stderr, "testserver:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// stdioConfig describes a server that is this test binary, re-executed.
func stdioConfig(t *testing.T, opts testserver.Options) ServerConfig {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	env := map[string]string{}
	for _, kv := range opts.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return ServerConfig{Name: opts.ServerName(), Transport: TransportStdio, Command: exe, Env: env}
}

// dialTest connects to a fixture server and closes it when the test ends.
func dialTest(t *testing.T, cfg ServerConfig) (*Client, *logBuffer) {
	t.Helper()
	log, buf := testLogger()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, cfg, log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, buf
}

// toolNamed finds one discovered tool by its registered name.
func toolNamed(t *testing.T, c *Client, name string) *Tool {
	t.Helper()
	for _, tool := range c.Tools() {
		if tool.Name() == name {
			return tool.(*Tool)
		}
	}
	t.Fatalf("no tool named %q among %v", name, toolNames(c))
	return nil
}

func toolNames(c *Client) []string {
	out := make([]string, 0, len(c.Tools()))
	for _, tool := range c.Tools() {
		out = append(out, tool.Name())
	}
	return out
}

// logBuffer collects log output for assertions about warnings. The lock is
// not decoration: a stdio server's stderr is drained by an os/exec goroutine
// that writes through the same logger the test reads.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func testLogger() (*slog.Logger, *logBuffer) {
	buf := &logBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}
