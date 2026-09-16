package main

import (
	"context"
	"flag"
	"io"
	"log/slog"

	"github.com/shreyasprasad/agentd/internal/tools"
	"github.com/shreyasprasad/agentd/internal/tools/mcp"
)

// defaultMCPConfig is where an operator is expected to declare MCP servers. It
// is a conventional path rather than a required one: LoadConfig treats a file
// that is not there as "no servers", so the default being absent is the normal
// case and not an error.
const defaultMCPConfig = "deploy/mcp.json"

type mcpFlags struct {
	path *string
}

// addMCPFlag registers -mcp-config on both serve and work. Both commands take
// it, and both are expected to be pointed at the same file, because the two
// registries have to agree on tool names: the API fills a run's default
// allowlist from its own registry and rejects unknown tools at submission
// (api.normalizeConfig), while the worker dispatches through its. A server
// declared to one process and not the other produces runs whose allowlist
// names a tool that cannot be dispatched.
func addMCPFlag(fs *flag.FlagSet) mcpFlags {
	return mcpFlags{
		path: fs.String("mcp-config", envOr("AGENTD_MCP_CONFIG", defaultMCPConfig),
			"MCP server config file; declared servers are discovered at boot and registered as external tools"),
	}
}

// connect dials the configured servers and returns their tools. The Closer
// shuts the sessions down, which for a stdio server means reaping its child
// process; callers must defer it on every exit path or leak one per run.
//
// serve connects too, even though it never invokes a tool. It needs the
// manifest to list and allowlist, and the only way to get a manifest is to ask
// the server for one.
func (f mcpFlags) connect(ctx context.Context, log *slog.Logger) ([]tools.Tool, io.Closer, error) {
	cfg, err := mcp.LoadConfig(*f.path)
	if err != nil {
		return nil, nil, err
	}
	if len(cfg.Servers) == 0 {
		return nil, io.NopCloser(nil), nil
	}
	return mcp.Connect(ctx, cfg, log)
}

// registerMCP adds discovered tools to a registry that already holds the
// builtins.
//
// Register, not MustRegister: the builtins are ours and a duplicate among them
// is a wiring bug worth panicking on, but these names came from a peer. A
// server that advertises a tool this process cannot register — a name that
// collides, a schema that will not compile — must cost that one tool and not
// the process, which is the same reasoning that makes an unreachable server a
// warning rather than a fatal error.
func registerMCP(reg *tools.Registry, ts []tools.Tool, log *slog.Logger) {
	for _, t := range ts {
		if err := reg.Register(t); err != nil {
			log.Warn("mcp tool not registered", "tool", t.Name(), "error", err)
		}
	}
}
