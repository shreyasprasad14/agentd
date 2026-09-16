package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadConfigParsesBothTransports(t *testing.T) {
	path := writeConfig(t, `{
      "servers": [
        {"name": "legal", "transport": "stdio", "command": "/usr/bin/legal-mcp",
         "args": ["--quiet"], "env": {"LEGAL_TOKEN": "abc"}, "timeout": "45s"},
        {"name": "filings", "transport": "http", "url": "https://mcp.example.test/v1"}
      ]
    }`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 2)

	require.Equal(t, TransportStdio, cfg.Servers[0].Transport)
	require.Equal(t, []string{"--quiet"}, cfg.Servers[0].Args)
	require.Equal(t, []string{"LEGAL_TOKEN=abc"}, cfg.Servers[0].envPairs())
	require.Equal(t, 45*time.Second, cfg.Servers[0].CallTimeout())

	require.Equal(t, TransportHTTP, cfg.Servers[1].Transport)
	// A server that says nothing about timeouts still gets one: an external
	// call with no bound would hold a cancel open forever.
	require.Equal(t, DefaultCallTimeout, cfg.Servers[1].CallTimeout())
}

func TestLoadConfigAbsentMeansNoServers(t *testing.T) {
	for _, path := range []string{"", "   ", filepath.Join(t.TempDir(), "nope.json")} {
		cfg, err := LoadConfig(path)
		require.NoError(t, err, "path %q", path)
		require.Empty(t, cfg.Servers)
	}
}

func TestLoadConfigRejectsBadFiles(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json", `{`, "parse mcp config"},
		{"unknown field", `{"servers":[{"name":"a","transport":"stdio","commmand":"x"}]}`, "commmand"},
		{"missing name", `{"servers":[{"transport":"stdio","command":"x"}]}`, "name is required"},
		{"name has separator", `{"servers":[{"name":"a__b","transport":"stdio","command":"x"}]}`, "must not contain"},
		{"name has a dot", `{"servers":[{"name":"a.b","transport":"stdio","command":"x"}]}`, "must match"},
		{"name too long", `{"servers":[{"name":"` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `","transport":"stdio","command":"x"}]}`, "longer than"},
		{"duplicate names", `{"servers":[{"name":"a","transport":"stdio","command":"x"},{"name":"a","transport":"stdio","command":"y"}]}`, "duplicate server name"},
		{"no transport", `{"servers":[{"name":"a","command":"x"}]}`, "transport is required"},
		{"unknown transport", `{"servers":[{"name":"a","transport":"grpc"}]}`, "unknown transport"},
		{"stdio without command", `{"servers":[{"name":"a","transport":"stdio"}]}`, "command is required"},
		{"stdio with url", `{"servers":[{"name":"a","transport":"stdio","command":"x","url":"http://h"}]}`, "url is meaningless"},
		{"http without url", `{"servers":[{"name":"a","transport":"http"}]}`, "url is required"},
		{"http relative url", `{"servers":[{"name":"a","transport":"http","url":"/v1"}]}`, "absolute http"},
		{"http with command", `{"servers":[{"name":"a","transport":"http","url":"http://h","command":"x"}]}`, "meaningless"},
		{"timeout as a number", `{"servers":[{"name":"a","transport":"stdio","command":"x","timeout":30}]}`, "duration string"},
		{"timeout unparseable", `{"servers":[{"name":"a","transport":"stdio","command":"x","timeout":"soon"}]}`, "parse duration"},
		{"timeout negative", `{"servers":[{"name":"a","transport":"stdio","command":"x","timeout":"-5s"}]}`, "must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestDurationRoundTrips(t *testing.T) {
	d := Duration(90 * time.Second)
	encoded, err := d.MarshalJSON()
	require.NoError(t, err)
	require.JSONEq(t, `"1m30s"`, string(encoded))

	var back Duration
	require.NoError(t, back.UnmarshalJSON(encoded))
	require.Equal(t, d, back)
}

func TestEnvPairsAreSorted(t *testing.T) {
	// Sorted so a respawned subprocess gets a byte-identical environment to
	// the one it had before the reconnect.
	s := ServerConfig{Env: map[string]string{"Z": "1", "A": "2", "M": "3"}}
	require.Equal(t, []string{"A=2", "M=3", "Z=1"}, s.envPairs())
}
