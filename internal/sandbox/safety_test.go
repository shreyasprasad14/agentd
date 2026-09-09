package sandbox_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/sandbox"
)

// TestSafety is the spec §12 SAFETY category, run against the real daemon:
// each case is a hostile script and an assertion about what the sandbox
// let it do. Every case also checks the container was removed. The M5 eval
// harness lifts these into its scorecard; keep them table-shaped.
func TestSafety(t *testing.T) {
	type probe map[string]any
	cases := []struct {
		name    string
		script  string
		timeout time.Duration
		check   func(t *testing.T, out *sandbox.Output, p probe)
	}{
		{
			name: "egress",
			script: `
import json, socket
r = {}
try:
    socket.create_connection(("1.1.1.1", 80), timeout=3); r["tcp"] = "CONNECTED"
except OSError as e:
    r["tcp"] = "blocked: %s" % e
try:
    socket.gethostbyname("example.com"); r["dns"] = "RESOLVED"
except OSError as e:
    r["dns"] = "blocked: %s" % e
# Kernels with tunnel modules loaded (Docker Desktop's VM) stamp down,
# addressless stubs like tunl0 and gre0 into every new netns; what matters
# is that nothing but loopback is up, routed, or addressed.
r["up"] = sorted(n for _, n in socket.if_nameindex()
                if int(open("/sys/class/net/%s/flags" % n).read(), 16) & 1)
r["ipv4_routes"] = [l.split()[0] for l in open("/proc/net/route").read().splitlines()[1:]]
r["ipv6_devices"] = sorted({l.split()[-1] for l in open("/proc/net/if_inet6").read().splitlines()})
print(json.dumps(r))
`,
			check: func(t *testing.T, out *sandbox.Output, p probe) {
				require.Equal(t, 0, out.ExitCode, "stderr: %s", out.Stderr)
				require.Contains(t, p["tcp"], "blocked")
				require.Contains(t, p["dns"], "blocked")
				require.Equal(t, []any{"lo"}, p["up"], "only loopback may be up")
				require.Empty(t, p["ipv4_routes"], "no IPv4 routes at all")
				require.Subset(t, []any{"lo"}, p["ipv6_devices"], "no IPv6 address outside loopback")
			},
		},
		{
			name: "fork bomb",
			// Every process forks until the pids cgroup refuses. The root
			// reports how far it got and exits nonzero; its exit ends the
			// container regardless of what the children are doing.
			script: `
import os, sys
forks = 0
try:
    while True:
        if os.fork() == 0:
            continue
        forks += 1
except BlockingIOError as e:
    print("fork refused after %d: %s" % (forks, e), file=sys.stderr)
    sys.exit(3)
`,
			timeout: 20 * time.Second,
			check: func(t *testing.T, out *sandbox.Output, _ probe) {
				require.False(t, out.TimedOut, "the bomb must be refused, not merely killed at the deadline")
				require.Equal(t, 3, out.ExitCode)
				require.Contains(t, string(out.Stderr), "fork refused after")
				require.Contains(t, string(out.Stderr), "Resource temporarily unavailable")
			},
		},
		{
			name: "filesystem",
			script: `
import errno, json, os
r = {}
for path in ["/x", "/usr/x", "/etc/x", "/work/in/x", "/work/in/main.py"]:
    try:
        open(path, "a").close(); r[path] = "WRITABLE"
    except OSError as e:
        r[path] = errno.errorcode[e.errno]
open("/tmp/ok", "w").write("scratch is fine"); r["/tmp/ok"] = open("/tmp/ok").read()
try:
    with open("/tmp/big", "wb") as f:
        for _ in range(100): f.write(b"\0" * (1 << 20))
    r["/tmp/100MiB"] = "WRITTEN"
except OSError as e:
    r["/tmp/100MiB"] = errno.errorcode[e.errno]
print(json.dumps(r))
`,
			check: func(t *testing.T, out *sandbox.Output, p probe) {
				require.Equal(t, 0, out.ExitCode, "stderr: %s", out.Stderr)
				for _, path := range []string{"/x", "/usr/x", "/etc/x"} {
					require.Equal(t, "EROFS", p[path], path)
				}
				require.Equal(t, "EACCES", p["/work/in/x"], "input dir is root-owned")
				require.Equal(t, "EACCES", p["/work/in/main.py"], "the script cannot rewrite itself")
				require.Equal(t, "scratch is fine", p["/tmp/ok"])
				require.Equal(t, "ENOSPC", p["/tmp/100MiB"], "tmpfs is capped at 64m")
			},
		},
		{
			name: "privileges",
			script: `
import json, os
status = dict(line.split(":", 1) for line in open("/proc/self/status") if ":" in line)
print(json.dumps({
    "uid": os.getuid(), "gid": os.getgid(), "euid": os.geteuid(),
    "cap_eff": status["CapEff"].strip(), "cap_prm": status["CapPrm"].strip(),
    "cap_bnd": status["CapBnd"].strip(), "no_new_privs": status["NoNewPrivs"].strip(),
}))
`,
			check: func(t *testing.T, out *sandbox.Output, p probe) {
				require.Equal(t, 0, out.ExitCode, "stderr: %s", out.Stderr)
				require.EqualValues(t, 65534, p["uid"])
				require.EqualValues(t, 65534, p["gid"])
				require.EqualValues(t, 65534, p["euid"])
				require.Equal(t, "0000000000000000", p["cap_eff"])
				require.Equal(t, "0000000000000000", p["cap_prm"])
				require.Equal(t, "0000000000000000", p["cap_bnd"], "bounding set is empty: no-new-privileges plus cap-drop ALL")
				require.Equal(t, "1", p["no_new_privs"])
			},
		},
		{
			name: "memory",
			script: `
import sys
chunks = []
try:
    for _ in range(64):
        chunks.append(bytearray(32 << 20))   # 2 GiB total against a 512 MiB cap
        chunks[-1][::4096] = b"x" * len(chunks[-1][::4096])  # touch every page
    print("ALLOCATED 2GiB")
except MemoryError:
    print("MemoryError", file=sys.stderr); sys.exit(4)
`,
			timeout: 60 * time.Second,
			check: func(t *testing.T, out *sandbox.Output, _ probe) {
				require.NotContains(t, string(out.Stdout), "ALLOCATED")
				require.True(t, out.OOMKilled || out.ExitCode == 4, "exit=%d oom=%v stderr=%s", out.ExitCode, out.OOMKilled, out.Stderr)
				require.False(t, out.TimedOut)
			},
		},
		{
			name: "no setuid escalation",
			// With no-new-privileges, exec of a setuid binary must not gain
			// privileges. su is setuid root in the base image.
			script: `
import os, subprocess, sys
p = subprocess.run(["/bin/su", "-c", "id -u", "root"], capture_output=True, text=True)
print("rc=%d out=%r err=%r" % (p.returncode, p.stdout.strip(), p.stderr.strip()))
sys.exit(0 if p.stdout.strip() != "0" else 9)
`,
			check: func(t *testing.T, out *sandbox.Output, _ probe) {
				require.Equal(t, 0, out.ExitCode, "stdout: %s stderr: %s", out.Stdout, out.Stderr)
			},
		},
	}

	d := newExecutor(t, sandbox.Limits{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := python(t, tc.script)
			spec.Timeout = tc.timeout
			begin := time.Now()
			out, err := d.Run(context.Background(), spec)
			require.NoError(t, err)
			t.Logf("%s: exit=%d timed_out=%v oom=%v in %s\nstdout: %s\nstderr: %s",
				tc.name, out.ExitCode, out.TimedOut, out.OOMKilled, time.Since(begin).Round(time.Millisecond), out.Stdout, out.Stderr)

			var p probe
			if len(out.Stdout) > 0 && out.Stdout[0] == '{' {
				require.NoError(t, json.Unmarshal(out.Stdout, &p))
			}
			tc.check(t, out, p)
			assertNoContainers(t)
		})
	}
}
