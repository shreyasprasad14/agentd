package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TransportKind names the wire one server is reached over. Both of spec §8's
// transports are here because the second one costs a struct literal once the
// first works.
type TransportKind string

const (
	// TransportStdio spawns the server as a subprocess and speaks
	// newline-delimited JSON over its pipes. This is the transport the
	// subprocess lifecycle rules in Client apply to.
	TransportStdio TransportKind = "stdio"

	// TransportHTTP speaks the streamable HTTP transport to an already
	// running server. Nothing is spawned, so nothing needs reaping.
	TransportHTTP TransportKind = "http"
)

// maxServerNameBytes bounds a server name so a namespaced tool name still has
// room under MaxToolNameBytes. Catching it in the config is the difference
// between an error an operator can read and a server that silently
// contributes no tools.
const maxServerNameBytes = 24

// serverNamePattern is what a server name may contain. It is the model APIs'
// tool-name alphabet minus the dot, because the whole namespaced name has to
// satisfy that alphabet and the server half is the half we control.
var serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Config is the parsed MCP server config file: the operator's declaration of
// which out-of-process peers this runtime trusts. It is deliberately the only
// place servers can be declared — a run that could name its own would invert
// the trust boundary the package doc draws.
type Config struct {
	Servers []ServerConfig `json:"servers"`
}

// ServerConfig declares one server. Command/Args/Env apply to
// TransportStdio, URL to TransportHTTP, and Timeout to both.
type ServerConfig struct {
	// Name is the namespace every tool this server advertises registers
	// under, and the name a run's allowlist uses to grant one.
	Name string `json:"name"`

	// Transport is "stdio" or "http".
	Transport TransportKind `json:"transport"`

	// Command is the binary to spawn for a stdio server.
	Command string `json:"command,omitempty"`

	// Args are the arguments it is spawned with.
	Args []string `json:"args,omitempty"`

	// Env are extra environment entries for the child, layered over the
	// worker's own environment rather than replacing it. A clean environment
	// would make every config restate PATH and HOME, and withholding the
	// operator's environment from a binary the operator chose to run buys
	// nothing: the trust decision was already made by listing it here.
	Env map[string]string `json:"env,omitempty"`

	// URL is the endpoint of an http server.
	URL string `json:"url,omitempty"`

	// Timeout bounds one tools/call. Zero means DefaultCallTimeout.
	Timeout Duration `json:"timeout,omitempty"`
}

// Duration is a time.Duration that decodes from a Go duration string
// ("30s", "2m"). encoding/json would otherwise read a bare nanosecond count,
// which nobody writes correctly by hand in a file meant to be edited by hand.
type Duration time.Duration

// Duration returns the underlying value.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// MarshalJSON writes the string form, so a config this package round-trips
// still reads the way an operator wrote it.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON parses the string form.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("want a duration string like \"30s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	if parsed <= 0 {
		return fmt.Errorf("duration %q must be positive", s)
	}
	*d = Duration(parsed)
	return nil
}

// LoadConfig reads a server config file.
//
// An empty path, or a path that does not exist, means no servers and no
// error: MCP is optional infrastructure, and the flag that carries this path
// has a conventional default that most deployments will not have populated —
// the same reasoning that makes an unreachable Docker daemon a warning in
// buildSandbox rather than a fatal error. A file that *does* exist and is
// malformed is still a hard error, which is where an operator's typo actually
// gets caught, and unknown keys are rejected for the same reason: a
// misspelled "commmand" that silently produced a server with no command would
// be a worse outcome than refusing to start.
func LoadConfig(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		return Config{}, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read mcp config %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse mcp config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("mcp config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks every server and rejects duplicate names. It runs at load
// and again in Connect, because a Config can also be built in code.
func (c Config) Validate() error {
	seen := make(map[string]bool, len(c.Servers))
	for i, s := range c.Servers {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("servers[%d]: %w", i, err)
		}
		if seen[s.Name] {
			// Two servers under one name would produce two tools under one
			// registered name, which Registry.Register refuses at boot. Catch
			// it in the file, where the fix is obvious.
			return fmt.Errorf("servers[%d]: duplicate server name %q", i, s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

// Validate checks one server declaration.
func (s ServerConfig) Validate() error {
	switch {
	case s.Name == "":
		return errors.New("name is required")
	case len(s.Name) > maxServerNameBytes:
		return fmt.Errorf("name %q is longer than %d bytes; it is a prefix on every tool name this server contributes",
			s.Name, maxServerNameBytes)
	case !serverNamePattern.MatchString(s.Name):
		return fmt.Errorf("name %q must match %s, because it is half of a tool name the model API has to accept",
			s.Name, serverNamePattern)
	case strings.Contains(s.Name, Separator):
		// Without this, "a__b" plus tool "c" and "a" plus tool "b__c" both
		// register as "a__b__c" and SplitName cannot tell them apart.
		return fmt.Errorf("name %q must not contain %q, which separates the server from the tool", s.Name, Separator)
	}

	switch s.Transport {
	case TransportStdio:
		if s.Command == "" {
			return fmt.Errorf("server %q: command is required for the %s transport", s.Name, TransportStdio)
		}
		if s.URL != "" {
			return fmt.Errorf("server %q: url is meaningless for the %s transport", s.Name, TransportStdio)
		}
	case TransportHTTP:
		if s.URL == "" {
			return fmt.Errorf("server %q: url is required for the %s transport", s.Name, TransportHTTP)
		}
		u, err := url.Parse(s.URL)
		if err != nil {
			return fmt.Errorf("server %q: parse url: %w", s.Name, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("server %q: url %q must be an absolute http or https URL", s.Name, s.URL)
		}
		if s.Command != "" || len(s.Args) > 0 {
			return fmt.Errorf("server %q: command and args are meaningless for the %s transport", s.Name, TransportHTTP)
		}
	case "":
		return fmt.Errorf("server %q: transport is required (%q or %q)", s.Name, TransportStdio, TransportHTTP)
	default:
		return fmt.Errorf("server %q: unknown transport %q (want %q or %q)", s.Name, s.Transport, TransportStdio, TransportHTTP)
	}
	return nil
}

// CallTimeout is the per-call bound this server runs under.
func (s ServerConfig) CallTimeout() time.Duration {
	if d := s.Timeout.Duration(); d > 0 {
		return d
	}
	return DefaultCallTimeout
}

// envPairs renders Env as KEY=VALUE, sorted so a respawned subprocess gets a
// byte-identical environment to the one it had before — an MCP server that
// hashes its own config should not see it change across a reconnect.
func (s ServerConfig) envPairs() []string {
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+s.Env[k])
	}
	return out
}
