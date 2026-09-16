// Command mcp-testserver runs the in-repo fixture MCP server over stdio, so
// the adapter can be exercised by hand the way an operator would wire a real
// third-party server:
//
//	go build -o /tmp/mcp-testserver ./internal/tools/mcp/testserver/cmd/mcp-testserver
//	cat > mcp.json <<'JSON'
//	{"servers":[{"name":"legal","transport":"stdio","command":"/tmp/mcp-testserver"}]}
//	JSON
//	agentd serve -mcp-config mcp.json
//
// The adapter's own tests do not use this binary — they re-execute the test
// binary instead, so `go test` needs no build step and no toolchain call of
// its own. This exists for the demo and for anyone who wants to point a
// different MCP client at it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/shreyasprasad/agentd/internal/tools/mcp/testserver"
)

func main() {
	// Environment first, flags over the top: the environment is how a parent
	// process configures a spawned server, and a hand-run server wants flags.
	opts, _ := testserver.OptionsFromEnv()
	fs := flag.NewFlagSet("mcp-testserver", flag.ExitOnError)
	fs.StringVar(&opts.Name, "name", opts.Name, "server implementation name")
	fs.BoolVar(&opts.PoisonedDescription, "poisoned-description", opts.PoisonedDescription,
		"advertise a tool description containing an instruction aimed at the model")
	fs.BoolVar(&opts.OversizedDescription, "oversized-description", opts.OversizedDescription,
		"advertise a tool description far past any reasonable cap")
	fs.BoolVar(&opts.HostileSchema, "hostile-schema", opts.HostileSchema,
		"advertise a tool whose input schema will not compile")
	fs.BoolVar(&opts.Crashable, "crashable", opts.Crashable,
		"expose crash_server, which exits the process without answering")
	fs.BoolVar(&opts.Stallable, "stallable", opts.Stallable,
		"expose stall, which never answers")
	fs.BoolVar(&opts.LongToolName, "long-tool-name", opts.LongToolName,
		"advertise a tool whose namespaced name is too long for the model APIs")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := testserver.Run(ctx, opts); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-testserver:", err)
		os.Exit(1)
	}
}
