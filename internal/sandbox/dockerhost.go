package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/moby/moby/client"
)

// DockerHost resolves the daemon address the way the docker CLI does, so a
// worker on a developer machine finds Docker Desktop, OrbStack, or Colima
// without configuration: DOCKER_HOST wins; otherwise the current context
// (DOCKER_CONTEXT, then currentContext in config.json) names an endpoint;
// otherwise the platform default socket. The SDK alone only knows the first
// and last of those.
func DockerHost() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return client.DefaultDockerHost
		}
		dir = filepath.Join(home, ".docker")
	}
	name := os.Getenv("DOCKER_CONTEXT")
	if name == "" {
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		if raw, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
			_ = json.Unmarshal(raw, &cfg)
		}
		name = cfg.CurrentContext
	}
	if name == "" || name == "default" {
		return client.DefaultDockerHost
	}
	if h := contextHost(dir, name); h != "" {
		return h
	}
	return client.DefaultDockerHost
}

// contextHost reads the docker endpoint of a named CLI context. Context
// metadata lives at <config>/contexts/meta/<sha256(name)>/meta.json.
func contextHost(dir, name string) string {
	sum := sha256.Sum256([]byte(name))
	raw, err := os.ReadFile(filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	return meta.Endpoints["docker"].Host
}
