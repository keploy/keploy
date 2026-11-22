package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"go.uber.org/zap"
)

// The variables the Docker Engine API clients read (client.FromEnv, which
// every client keploy and the compose library build use), and the docker
// CLI's own.
const (
	envDockerHost      = "DOCKER_HOST"
	envDockerCertPath  = "DOCKER_CERT_PATH"
	envDockerTLSVerify = "DOCKER_TLS_VERIFY"
	envDockerContext   = "DOCKER_CONTEXT"
	envDockerConfig    = "DOCKER_CONFIG"
)

// ContextEnv names the docker context keploy took DOCKER_HOST from
// (UseDockerContext), so that a message about the endpoint can say where it
// came from rather than blame a DOCKER_HOST the user never set. It goes
// through the sudo re-exec with DOCKER_HOST. Read it with DockerHostContext,
// which says it only while DOCKER_HOST is still the one keploy took from
// that context.
const ContextEnv = "KEPLOY_DOCKER_CONTEXT"

// ContextHostEnv is the DOCKER_HOST keploy set with ContextEnv; a test that
// stands for keploy sets both. A ContextEnv is keploy's only while
// DOCKER_HOST is still this one: one exported in the user's shell, or
// inherited from a keploy that ran under another DOCKER_HOST, would otherwise
// say a DOCKER_HOST the user set came from a context.
const ContextHostEnv = "KEPLOY_DOCKER_CONTEXT_HOST"

// defaultContextName is the docker CLI's built-in context: DOCKER_HOST, else
// the platform's default socket, which is what client.FromEnv reaches too.
const defaultContextName = "default"

// DockerContext is the Docker endpoint of one docker context, as the docker
// CLI's context store holds it.
type DockerContext struct {
	// Name is the context's name, such as rancher-desktop.
	Name string
	// Host is its Engine API endpoint, such as unix:///home/u/.rd/docker.sock.
	Host string
	// TLSDir is the directory of its TLS material (ca.pem, cert.pem,
	// key.pem), whether or not any of it is there.
	TLSDir string
	// SkipTLSVerify is the context's skip-tls-verify.
	SkipTLSVerify bool
}

// CurrentDockerContext is the docker context the docker CLI uses, chosen as
// the CLI chooses it (resolveContextName in docker/cli): DOCKER_CONTEXT, else
// the currentContext of $DOCKER_CONFIG/config.json (~/.docker/config.json).
// ok is false when the CLI uses no context of its own: DOCKER_HOST is set,
// which the CLI takes over any context, or the context is the default one, or
// it names no endpoint. Both of those are what client.FromEnv reaches as well.
//
// A context that is named but cannot be read is an error: the docker CLI
// refuses to run in it, so no endpoint keploy picked would be the CLI's.
func CurrentDockerContext() (c DockerContext, ok bool, err error) {
	if os.Getenv(envDockerHost) != "" {
		return DockerContext{}, false, nil
	}
	dir := dockerConfigDir()
	name := os.Getenv(envDockerContext)
	if name == "" {
		if name, err = currentContextName(dir); err != nil {
			return DockerContext{}, false, err
		}
	}
	if name == "" || name == defaultContextName {
		return DockerContext{}, false, nil
	}

	id := contextDirOf(name)
	metaPath := filepath.Join(dir, "contexts", "meta", id, "meta.json")
	b, err := os.ReadFile(metaPath)
	if errors.Is(err, fs.ErrNotExist) {
		return DockerContext{}, false, fmt.Errorf("docker context %q does not exist (no %s)", name, metaPath)
	}
	if err != nil {
		return DockerContext{}, false, fmt.Errorf("read docker context %q: %w", name, err)
	}
	var meta struct {
		Endpoints map[string]struct {
			Host          string `json:"Host"`
			SkipTLSVerify bool   `json:"SkipTLSVerify"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return DockerContext{}, false, fmt.Errorf("read docker context %q from %s: %w", name, metaPath, err)
	}
	ep := meta.Endpoints["docker"]
	if ep.Host == "" {
		return DockerContext{}, false, nil
	}
	return DockerContext{
		Name:          name,
		Host:          ep.Host,
		TLSDir:        filepath.Join(dir, "contexts", "tls", id, "docker"),
		SkipTLSVerify: ep.SkipTLSVerify,
	}, true, nil
}

// Environment is what the Engine API's environment variables must hold for
// client.FromEnv to reach c as the docker CLI does: DOCKER_HOST, and the TLS
// pair, set from c's TLS material or left empty when it has none. It is an
// error when the variables cannot say what c says: an ssh endpoint, which
// only the docker CLI dials, or TLS material that is not exactly ca.pem,
// cert.pem and key.pem.
func (c DockerContext) Environment() (map[string]string, error) {
	scheme, _, found := strings.Cut(c.Host, "://")
	if !found {
		return nil, fmt.Errorf("docker context %q has the endpoint %q, which names no scheme", c.Name, c.Host)
	}
	switch scheme {
	case "unix", "npipe", "tcp", "http", "https":
	default:
		return nil, fmt.Errorf("docker context %q has a %s endpoint (%s), which only the docker CLI can dial, not keploy's Engine API client", c.Name, scheme, c.Host)
	}

	var have, missing []string
	for _, f := range []string{"ca.pem", "cert.pem", "key.pem"} {
		if _, err := os.Stat(filepath.Join(c.TLSDir, f)); err == nil {
			have = append(have, f)
		} else {
			missing = append(missing, f)
		}
	}
	env := map[string]string{
		envDockerHost:      c.Host,
		envDockerCertPath:  "",
		envDockerTLSVerify: "",
		ContextEnv:         c.Name,
		ContextHostEnv:     c.Host,
	}
	switch {
	case len(missing) == 0:
		env[envDockerCertPath] = c.TLSDir
		if !c.SkipTLSVerify {
			env[envDockerTLSVerify] = "1"
		}
	case len(have) > 0:
		return nil, fmt.Errorf("docker context %q has only %s of its TLS material in %s, and DOCKER_CERT_PATH needs all of ca.pem, cert.pem and key.pem",
			c.Name, strings.Join(have, ", "), c.TLSDir)
	case c.SkipTLSVerify:
		return nil, fmt.Errorf("docker context %q skips TLS verification with no TLS material, which DOCKER_CERT_PATH cannot say", c.Name)
	}
	return env, nil
}

// UseDockerContext points the Engine API clients keploy builds at the daemon
// the docker CLI uses.
//
// Those clients (keploy's own, the compose library's, the cloud replay's
// preflight) read DOCKER_HOST alone, else the platform's default socket, and
// never the docker CLI's current context. A daemon reached only through a
// context is then out of keploy's reach: Rancher Desktop
// (~/.rd/docker.sock), Docker Desktop for Linux, colima, OrbStack and
// rootless Docker all put theirs away from the default socket, and the
// docker CLI finds it through their context. So when DOCKER_HOST is not set
// and the current context is not the default one, this sets DOCKER_HOST (and
// the TLS pair) from it, as the CLI would resolve it. Every client built
// afterwards, and every process keploy starts, the `docker` CLI and the root
// keploy of the sudo re-exec among them, reaches that same daemon.
//
// Call it once at startup, before the sudo re-exec: the re-executed keploy
// runs as root, whose docker configuration does not name the user's context.
// A context that the variables cannot express changes nothing, and is said.
//
// A ContextEnv that does not go with this DOCKER_HOST is cleared first
// (clearStaleContextEnv), so that after this call ContextEnv is set only
// while DOCKER_HOST is keploy's.
func UseDockerContext(logger *zap.Logger) {
	clearStaleContextEnv(logger)
	c, ok, err := CurrentDockerContext()
	if err != nil {
		logger.Warn("keploy could not read the docker CLI's current context, so it reaches the docker daemon through DOCKER_HOST, else the default socket",
			zap.Error(err),
			zap.String("next_step", "fix the context (`docker context ls`), or set DOCKER_HOST to the daemon's endpoint"))
		return
	}
	if !ok {
		return
	}
	env, err := c.Environment()
	if err != nil {
		logger.Warn("keploy cannot reach the docker daemon of the docker CLI's current context, so it reaches it through DOCKER_HOST, else the default socket",
			zap.String("context", c.Name),
			zap.Error(err),
			zap.String("next_step", "set DOCKER_HOST (and DOCKER_CERT_PATH, DOCKER_TLS_VERIFY for TLS) to a daemon keploy can reach"))
		return
	}
	// The markers go last: a write that fails part-way leaves no marker
	// for a DOCKER_HOST that is not the context's.
	for _, k := range []string{envDockerHost, envDockerCertPath, envDockerTLSVerify, ContextHostEnv, ContextEnv} {
		if v := env[k]; v == "" {
			err = os.Unsetenv(k)
		} else {
			err = os.Setenv(k, v)
		}
		if err != nil {
			logger.Warn("keploy could not take the docker CLI's current context for its docker daemon",
				zap.String("context", c.Name), zap.String("variable", k), zap.Error(err))
			return
		}
	}
	logger.Debug("reaching the docker daemon of the docker CLI's current context",
		zap.String("context", c.Name), zap.String("endpoint", c.Host))
}

// DockerHostContext is the docker context keploy took DOCKER_HOST from
// (UseDockerContext), "" when DOCKER_HOST is not keploy's from a context:
// the user set it, an engine keploy prepared set it (Prepare), or there is
// none. It reads ContextEnv only while DOCKER_HOST is still the endpoint
// keploy set with it, whatever changed the environment since.
func DockerHostContext() string {
	name, host := os.Getenv(ContextEnv), os.Getenv(ContextHostEnv)
	if name == "" || host == "" || host != os.Getenv(envDockerHost) {
		return ""
	}
	return name
}

// clearStaleContextEnv unsets ContextEnv and ContextHostEnv unless they go
// with DOCKER_HOST (DockerHostContext): keploy set the pair with it, in this
// process or, for the keploy the sudo re-exec starts, in the one that ran it
// (the re-exec keeps the whole environment). A pair that does not go with
// it would say to the processes keploy starts that the user's DOCKER_HOST
// came from a context.
func clearStaleContextEnv(logger *zap.Logger) {
	if DockerHostContext() != "" {
		return
	}
	clearContextEnv(logger)
}

// clearContextEnv unsets ContextEnv and ContextHostEnv.
func clearContextEnv(logger *zap.Logger) {
	for _, k := range []string{ContextEnv, ContextHostEnv} {
		if err := os.Unsetenv(k); err != nil {
			logger.Debug("keploy could not clear a docker context marker",
				zap.String("variable", k), zap.Error(err))
		}
	}
}

// dockerConfigDir is the docker CLI's configuration directory (config.Dir in
// docker/cli): DOCKER_CONFIG, else .docker in the home directory.
func dockerConfigDir() string {
	if dir := os.Getenv(envDockerConfig); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	if home == "" && runtime.GOOS != "windows" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	return filepath.Join(home, ".docker")
}

// currentContextName is config.json's currentContext in dir, "" when there
// is no config.json or it names none.
func currentContextName(dir string) (string, error) {
	path := filepath.Join(dir, "config.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the docker CLI's configuration: %w", err)
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", fmt.Errorf("read the docker CLI's configuration %s: %w", path, err)
	}
	return cfg.CurrentContext, nil
}

// contextDirOf is the directory name the context store keeps a context under:
// the hex SHA-256 of its name (contextdirOf in docker/cli).
func contextDirOf(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}
