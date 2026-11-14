package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// dockerConfig is a docker CLI configuration directory for one test, made the
// one the CLI and keploy read, with the Engine API variables cleared.
func dockerConfig(t *testing.T, current string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(envDockerConfig, dir)
	for _, k := range []string{envDockerHost, envDockerContext, envDockerCertPath, envDockerTLSVerify, ContextEnv, ContextHostEnv} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	if current != "" {
		writeFile(t, filepath.Join(dir, "config.json"), `{"auths":{},"currentContext":"`+current+`"}`)
	}
	return dir
}

// addContext writes a context into the store the way `docker context create`
// does, and returns its TLS directory.
func addContext(t *testing.T, dir, name, host string, skipTLSVerify bool) string {
	t.Helper()
	skip := "false"
	if skipTLSVerify {
		skip = "true"
	}
	id := contextDirOf(name)
	writeFile(t, filepath.Join(dir, "contexts", "meta", id, "meta.json"),
		`{"Name":"`+name+`","Metadata":{"Description":""},"Endpoints":{"docker":{"Host":"`+host+`","SkipTLSVerify":`+skip+`}}}`)
	return filepath.Join(dir, "contexts", "tls", id, "docker")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The store keeps a context under the hex SHA-256 of its name, as docker/cli's
// contextdirOf (digest.FromString(name).Encoded()) names it: the directory
// `docker context create rancher-desktop` (docker 29) made.
func TestContextDirOfIsTheStoresName(t *testing.T) {
	if got, want := contextDirOf("rancher-desktop"), "b547d66a5de60e5f0843aba28283a8875c2ad72e99ba076060ef9ec7c09917c8"; got != want {
		t.Fatalf("contextDirOf(\"rancher-desktop\") = %q, want %q", got, want)
	}
}

// Rancher Desktop makes its context the current one and serves its daemon at
// ~/.rd/docker.sock: that is the endpoint the docker CLI, and so keploy, uses.
func TestTheCurrentContextIsTheCLIs(t *testing.T) {
	dir := dockerConfig(t, "rancher-desktop")
	tlsDir := addContext(t, dir, "rancher-desktop", "unix:///home/u/.rd/docker.sock", false)

	c, ok, err := CurrentDockerContext()
	if err != nil || !ok {
		t.Fatalf("CurrentDockerContext() = %+v, %v, %v", c, ok, err)
	}
	if c.Name != "rancher-desktop" || c.Host != "unix:///home/u/.rd/docker.sock" || c.TLSDir != tlsDir || c.SkipTLSVerify {
		t.Fatalf("CurrentDockerContext() = %+v", c)
	}
	env, err := c.Environment()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		envDockerHost:      "unix:///home/u/.rd/docker.sock",
		envDockerCertPath:  "",
		envDockerTLSVerify: "",
		ContextEnv:         "rancher-desktop",
		ContextHostEnv:     "unix:///home/u/.rd/docker.sock",
	}
	if len(env) != len(want) {
		t.Fatalf("Environment() = %v, want %v", env, want)
	}
	for k, v := range want {
		if env[k] != v {
			t.Fatalf("Environment()[%s] = %q, want %q", k, env[k], v)
		}
	}
}

// The docker CLI takes DOCKER_HOST over any context, DOCKER_CONTEXT over
// config.json's, and uses no context of its own for the default one or when
// none is named.
func TestTheCLIsOrderOfChoice(t *testing.T) {
	t.Run("DOCKER_HOST wins and nothing is read", func(t *testing.T) {
		dir := dockerConfig(t, "rancher-desktop")
		writeFile(t, filepath.Join(dir, "config.json"), `not json`)
		t.Setenv(envDockerHost, "tcp://10.0.0.1:2375")
		if c, ok, err := CurrentDockerContext(); ok || err != nil {
			t.Fatalf("CurrentDockerContext() = %+v, %v, %v", c, ok, err)
		}
	})
	t.Run("DOCKER_CONTEXT over config.json", func(t *testing.T) {
		dir := dockerConfig(t, "rancher-desktop")
		addContext(t, dir, "rancher-desktop", "unix:///home/u/.rd/docker.sock", false)
		addContext(t, dir, "colima", "unix:///home/u/.colima/default/docker.sock", false)
		t.Setenv(envDockerContext, "colima")
		c, ok, err := CurrentDockerContext()
		if err != nil || !ok || c.Name != "colima" || c.Host != "unix:///home/u/.colima/default/docker.sock" {
			t.Fatalf("CurrentDockerContext() = %+v, %v, %v", c, ok, err)
		}
	})
	t.Run("no config.json", func(t *testing.T) {
		dockerConfig(t, "")
		if c, ok, err := CurrentDockerContext(); ok || err != nil {
			t.Fatalf("CurrentDockerContext() = %+v, %v, %v", c, ok, err)
		}
	})
	t.Run("config.json names no context", func(t *testing.T) {
		dir := dockerConfig(t, "")
		writeFile(t, filepath.Join(dir, "config.json"), `{"auths":{}}`)
		if c, ok, err := CurrentDockerContext(); ok || err != nil {
			t.Fatalf("CurrentDockerContext() = %+v, %v, %v", c, ok, err)
		}
	})
	t.Run("the default context", func(t *testing.T) {
		dockerConfig(t, "default")
		if c, ok, err := CurrentDockerContext(); ok || err != nil {
			t.Fatalf("CurrentDockerContext() = %+v, %v, %v", c, ok, err)
		}
	})
	t.Run("a context with no docker endpoint", func(t *testing.T) {
		dir := dockerConfig(t, "k8s-only")
		writeFile(t, filepath.Join(dir, "contexts", "meta", contextDirOf("k8s-only"), "meta.json"), `{"Name":"k8s-only","Endpoints":{}}`)
		if c, ok, err := CurrentDockerContext(); ok || err != nil {
			t.Fatalf("CurrentDockerContext() = %+v, %v, %v", c, ok, err)
		}
	})
}

// A context the CLI cannot use is an error, not a silent fall back to the
// default socket.
func TestAContextThatCannotBeReadIsAnError(t *testing.T) {
	t.Run("named but not in the store", func(t *testing.T) {
		dockerConfig(t, "rancher-desktop")
		_, ok, err := CurrentDockerContext()
		if ok || err == nil || !strings.Contains(err.Error(), `"rancher-desktop" does not exist`) {
			t.Fatalf("CurrentDockerContext() = %v, %v", ok, err)
		}
	})
	t.Run("config.json is not JSON", func(t *testing.T) {
		dir := dockerConfig(t, "")
		writeFile(t, filepath.Join(dir, "config.json"), `{`)
		if _, ok, err := CurrentDockerContext(); ok || err == nil {
			t.Fatalf("CurrentDockerContext() = %v, %v", ok, err)
		}
	})
}

// The TLS pair carries a context's TLS material only when it is all there;
// the variables cannot say anything else the context may.
func TestEnvironmentSaysWhatTheVariablesCan(t *testing.T) {
	newTLS := func(t *testing.T, skip bool, files ...string) DockerContext {
		dir := dockerConfig(t, "remote")
		tlsDir := addContext(t, dir, "remote", "tcp://10.0.0.1:2376", skip)
		for _, f := range files {
			writeFile(t, filepath.Join(tlsDir, f), "pem")
		}
		c, ok, err := CurrentDockerContext()
		if err != nil || !ok {
			t.Fatalf("CurrentDockerContext() = %+v, %v, %v", c, ok, err)
		}
		return c
	}
	t.Run("all of it, verified", func(t *testing.T) {
		c := newTLS(t, false, "ca.pem", "cert.pem", "key.pem")
		env, err := c.Environment()
		if err != nil || env[envDockerCertPath] != c.TLSDir || env[envDockerTLSVerify] != "1" {
			t.Fatalf("Environment() = %v, %v", env, err)
		}
	})
	t.Run("all of it, not verified", func(t *testing.T) {
		c := newTLS(t, true, "ca.pem", "cert.pem", "key.pem")
		env, err := c.Environment()
		if err != nil || env[envDockerCertPath] != c.TLSDir || env[envDockerTLSVerify] != "" {
			t.Fatalf("Environment() = %v, %v", env, err)
		}
	})
	t.Run("part of it", func(t *testing.T) {
		c := newTLS(t, false, "ca.pem")
		if env, err := c.Environment(); err == nil {
			t.Fatalf("Environment() = %v, want an error", env)
		}
	})
	t.Run("skip verification with none of it", func(t *testing.T) {
		c := newTLS(t, true)
		if env, err := c.Environment(); err == nil {
			t.Fatalf("Environment() = %v, want an error", env)
		}
	})
	t.Run("an ssh endpoint", func(t *testing.T) {
		c := DockerContext{Name: "remote", Host: "ssh://u@host", TLSDir: t.TempDir()}
		if env, err := c.Environment(); err == nil || !strings.Contains(err.Error(), "only the docker CLI can dial") {
			t.Fatalf("Environment() = %v, %v", env, err)
		}
	})
	t.Run("no scheme", func(t *testing.T) {
		c := DockerContext{Name: "odd", Host: "/var/run/docker.sock", TLSDir: t.TempDir()}
		if env, err := c.Environment(); err == nil {
			t.Fatalf("Environment() = %v, want an error", env)
		}
	})
}

// UseDockerContext sets the variables from the current context, clearing a
// TLS pair the context does not have, and changes nothing otherwise.
func TestUseDockerContext(t *testing.T) {
	t.Run("a context sets them", func(t *testing.T) {
		dir := dockerConfig(t, "rancher-desktop")
		addContext(t, dir, "rancher-desktop", "unix:///home/u/.rd/docker.sock", false)
		t.Setenv(envDockerCertPath, "/stale/certs")
		t.Setenv(envDockerTLSVerify, "1")
		UseDockerContext(zap.NewNop())
		if got := os.Getenv(envDockerHost); got != "unix:///home/u/.rd/docker.sock" {
			t.Fatalf("DOCKER_HOST = %q", got)
		}
		if got := os.Getenv(ContextEnv); got != "rancher-desktop" {
			t.Fatalf("%s = %q", ContextEnv, got)
		}
		if got := os.Getenv(ContextHostEnv); got != "unix:///home/u/.rd/docker.sock" {
			t.Fatalf("%s = %q", ContextHostEnv, got)
		}
		if got := DockerHostContext(); got != "rancher-desktop" {
			t.Fatalf("DockerHostContext() = %q, want rancher-desktop", got)
		}
		for _, k := range []string{envDockerCertPath, envDockerTLSVerify} {
			if v, set := os.LookupEnv(k); set {
				t.Fatalf("%s = %q, want it unset", k, v)
			}
		}
	})
	t.Run("DOCKER_HOST is left as it is", func(t *testing.T) {
		dir := dockerConfig(t, "rancher-desktop")
		addContext(t, dir, "rancher-desktop", "unix:///home/u/.rd/docker.sock", false)
		t.Setenv(envDockerHost, "unix:///run/user/1000/docker.sock")
		UseDockerContext(zap.NewNop())
		if got := os.Getenv(envDockerHost); got != "unix:///run/user/1000/docker.sock" {
			t.Fatalf("DOCKER_HOST = %q", got)
		}
		if got := os.Getenv(ContextEnv); got != "" {
			t.Fatalf("%s = %q", ContextEnv, got)
		}
	})
	// KEPLOY_DOCKER_CONTEXT says DOCKER_HOST is keploy's, taken from that
	// context. One that does not go with this DOCKER_HOST (exported in the
	// user's shell, or inherited, with its host, from a keploy that ran under
	// another DOCKER_HOST) would say it of the user's own, and a message built
	// on it would name a context for a DOCKER_HOST the user set. Both markers
	// are cleared, and DOCKER_HOST is the user's as it was.
	t.Run("one that does not go with DOCKER_HOST is cleared", func(t *testing.T) {
		for _, c := range []struct{ name, current, dockerHost, markedHost string }{
			{"the name alone, under the user's DOCKER_HOST", "rancher-desktop", "tcp://build-box:2376", ""},
			{"the name alone, under no DOCKER_HOST and the default context", "", "", ""},
			{"the name alone, under the context's own endpoint set by the user", "rancher-desktop", "unix:///home/u/.rd/docker.sock", ""},
			{"a pair for another DOCKER_HOST, under the user's", "rancher-desktop", "tcp://build-box:2376", "unix:///home/u/.rd/docker.sock"},
			{"a pair, under no DOCKER_HOST and the default context", "", "", "unix:///home/u/.rd/docker.sock"},
		} {
			t.Run(c.name, func(t *testing.T) {
				dir := dockerConfig(t, c.current)
				addContext(t, dir, "rancher-desktop", "unix:///home/u/.rd/docker.sock", false)
				if c.dockerHost != "" {
					t.Setenv(envDockerHost, c.dockerHost)
				}
				t.Setenv(ContextEnv, "rancher-desktop")
				if c.markedHost != "" {
					t.Setenv(ContextHostEnv, c.markedHost)
				}
				if got := DockerHostContext(); got != "" {
					t.Fatalf("DockerHostContext() = %q before UseDockerContext, want \"\": DOCKER_HOST (%q) is not keploy's", got, c.dockerHost)
				}
				UseDockerContext(zap.NewNop())
				for _, k := range []string{ContextEnv, ContextHostEnv} {
					if v, set := os.LookupEnv(k); set {
						t.Fatalf("%s = %q, want it unset: DOCKER_HOST (%q) is not keploy's", k, v, os.Getenv(envDockerHost))
					}
				}
				if got := os.Getenv(envDockerHost); got != c.dockerHost {
					t.Fatalf("DOCKER_HOST = %q, want %q", got, c.dockerHost)
				}
			})
		}
	})
	// A pair left for another context, with no DOCKER_HOST: the current
	// context sets DOCKER_HOST, and the pair names it.
	t.Run("a stale pair under another current context is that context's", func(t *testing.T) {
		dir := dockerConfig(t, "colima")
		addContext(t, dir, "colima", "unix:///home/u/.colima/default/docker.sock", false)
		t.Setenv(ContextEnv, "rancher-desktop")
		t.Setenv(ContextHostEnv, "unix:///home/u/.rd/docker.sock")
		UseDockerContext(zap.NewNop())
		if got := os.Getenv(envDockerHost); got != "unix:///home/u/.colima/default/docker.sock" {
			t.Fatalf("DOCKER_HOST = %q", got)
		}
		if got := DockerHostContext(); got != "colima" {
			t.Fatalf("DockerHostContext() = %q, want colima", got)
		}
	})
	// The keploy the sudo re-exec starts gets the whole environment keploy
	// set (utils.ReexecWithSudo): DOCKER_HOST and the pair that goes with it.
	// What keeps the pair is that DOCKER_HOST is still the one keploy set
	// with it, and it says the context.
	t.Run("the re-executed keploy keeps the pair keploy set", func(t *testing.T) {
		dir := dockerConfig(t, "rancher-desktop")
		addContext(t, dir, "rancher-desktop", "unix:///home/u/.rd/docker.sock", false)
		UseDockerContext(zap.NewNop())
		UseDockerContext(zap.NewNop())
		if got := os.Getenv(envDockerHost); got != "unix:///home/u/.rd/docker.sock" {
			t.Fatalf("DOCKER_HOST = %q", got)
		}
		if got := DockerHostContext(); got != "rancher-desktop" {
			t.Fatalf("DockerHostContext() = %q, want rancher-desktop", got)
		}
	})
	// The pair is read against DOCKER_HOST as it is now: one that changed
	// since keploy set it (a user's, an engine's) is no context's.
	t.Run("DockerHostContext says nothing once DOCKER_HOST changed", func(t *testing.T) {
		dir := dockerConfig(t, "rancher-desktop")
		addContext(t, dir, "rancher-desktop", "unix:///home/u/.rd/docker.sock", false)
		UseDockerContext(zap.NewNop())
		t.Setenv(envDockerHost, "tcp://build-box:2376")
		if got := DockerHostContext(); got != "" {
			t.Fatalf("DockerHostContext() = %q, want \"\"", got)
		}
	})
	t.Run("one the variables cannot say is said and changes nothing", func(t *testing.T) {
		dir := dockerConfig(t, "remote")
		addContext(t, dir, "remote", "ssh://u@host", false)
		core, logs := observer.New(zap.WarnLevel)
		UseDockerContext(zap.New(core))
		if v, set := os.LookupEnv(envDockerHost); set {
			t.Fatalf("DOCKER_HOST = %q, want it unset", v)
		}
		if logs.Len() != 1 || logs.All()[0].ContextMap()["context"] != "remote" {
			t.Fatalf("logged %v, want one warning naming the context", logs.All())
		}
	})
	t.Run("one that cannot be read is said and changes nothing", func(t *testing.T) {
		dockerConfig(t, "gone")
		core, logs := observer.New(zap.WarnLevel)
		UseDockerContext(zap.New(core))
		if v, set := os.LookupEnv(envDockerHost); set {
			t.Fatalf("DOCKER_HOST = %q, want it unset", v)
		}
		if logs.Len() != 1 {
			t.Fatalf("logged %v, want one warning", logs.All())
		}
	})
}
