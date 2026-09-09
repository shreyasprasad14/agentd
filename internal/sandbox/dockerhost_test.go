package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/client"
)

func TestDockerHostResolution(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	if got := DockerHost(); got != client.DefaultDockerHost {
		t.Fatalf("no config: %q", got)
	}

	writeContext := func(name, host string) {
		sum := sha256.Sum256([]byte(name))
		meta := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
		if err := os.MkdirAll(meta, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `","SkipTLSVerify":false}}}`
		if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeContext("desktop-linux", "unix:///home/me/.docker/run/docker.sock")
	writeContext("remote", "tcp://10.0.0.5:2376")

	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"desktop-linux"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DockerHost(); got != "unix:///home/me/.docker/run/docker.sock" {
		t.Fatalf("current context: %q", got)
	}

	t.Setenv("DOCKER_CONTEXT", "remote")
	if got := DockerHost(); got != "tcp://10.0.0.5:2376" {
		t.Fatalf("DOCKER_CONTEXT: %q", got)
	}

	t.Setenv("DOCKER_CONTEXT", "default")
	if got := DockerHost(); got != client.DefaultDockerHost {
		t.Fatalf("default context: %q", got)
	}

	t.Setenv("DOCKER_CONTEXT", "missing")
	if got := DockerHost(); got != client.DefaultDockerHost {
		t.Fatalf("unknown context falls back: %q", got)
	}

	t.Setenv("DOCKER_HOST", "ssh://build@box")
	if got := DockerHost(); got != "ssh://build@box" {
		t.Fatalf("DOCKER_HOST wins: %q", got)
	}
}
