package replay

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/testnet"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
)

func TestDockerPublishedHostPort(t *testing.T) {
	cases := []struct {
		name     string
		cmd      string
		wantHost string
		wantPort string
		wantOK   bool
	}{
		{"hostPort:containerPort", "docker run --name my-app -p 8080:8080 img", "127.0.0.1", "8080", true},
		{"ip:hostPort:containerPort", "docker run -p 127.0.0.1:9090:8080 img", "127.0.0.1", "9090", true},
		{"--publish long flag", "docker run --publish 5000:5000 img", "127.0.0.1", "5000", true},
		{"distinct host port", "docker run -p 18080:8080 img", "127.0.0.1", "18080", true},
		{"with /tcp suffix", "docker run -p 6379:6379/tcp img", "127.0.0.1", "6379", true},
		{"container-only random host port", "docker run -p 8080 img", "", "", false},
		{"port range not waitable", "docker run -p 8000-8005:8000-8005 img", "", "", false},
		{"native app, no -p", "node server.js", "", "", false},
		{"docker compose has no -p in cmd", "docker compose up", "", "", false},
		{"explicit host ip", "docker run -p 0.0.0.0:7000:7000 img", "0.0.0.0", "7000", true},
		// An IPv6 host ip, bracketed or not, as docker reads it. Split on its
		// colons, "[::1]:8080:8080" named host "[" and "::1:8080:80" host
		// 127.0.0.1, neither where the app listens.
		{"bracketed IPv6 host ip", "docker run -p [::1]:8080:8080 img", "::1", "8080", true},
		{"bracketed IPv6 host ip, distinct ports", "docker run -p [fd00::1]:18080:80 img", "fd00::1", "18080", true},
		{"IPv6 any address", "docker run -p [::]:8080:8080 img", "::", "8080", true},
		{"unbracketed IPv6 host ip", "docker run -p ::1:8080:80 img", "::1", "8080", true},
		{"bracketed IPv6 host ip with /tcp", "docker run -p [::1]:8080:8080/tcp img", "::1", "8080", true},
		{"bracketed IPv6 host ip, random host port", "docker run -p [::1]::8080 img", "", "", false},
		{"IPv4 host ip, random host port", "docker run -p 127.0.0.1::8080 img", "", "", false},
		{"host port 0 is docker's pick", "docker run -p 0:8080 img", "", "", false},
		{"a host port range for one container port", "docker run -p 8000-8005:80 img", "", "", false},
		// A UDP port never accepts the TCP connection the gate waits for.
		{"UDP port", "docker run -p 5353:5353/udp img", "", "", false},
		{"the TCP port after a UDP one", "docker run -p 5353:5353/udp -p 8080:80 img", "127.0.0.1", "8080", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, port, ok, err := dockerPublishedHostPort(c.cmd)
			if err != nil || ok != c.wantOK || host != c.wantHost || port != c.wantPort {
				t.Fatalf("dockerPublishedHostPort(%q) = (%q,%q,%v,%v), want (%q,%q,%v,nil)",
					c.cmd, host, port, ok, err, c.wantHost, c.wantPort, c.wantOK)
			}
		})
	}
}

// A mapping docker refuses names no host address. It is an error, never an
// address to wait on: split on its colons, "[::1]:8080" named host "[" and
// port "1]", where no wait ever ends. In single quotes the shell expands no
// variable, so docker is given "$KEPLOY_ROW_PORT:8080" as it is written.
func TestDockerPublishedHostPortRefusesAMappingDockerRefuses(t *testing.T) {
	t.Setenv("KEPLOY_ROW_PORT", "18080")
	for _, spec := range []string{"[::1]:8080", "localhost:8080:80", "abc:8080", "65536:80", "[fd00::1%eth0]:8080:80", "'$KEPLOY_ROW_PORT:8080'"} {
		cmd := "docker run -p " + spec + " -p 9090:9090 img"
		host, port, ok, err := dockerPublishedHostPort(cmd)
		if err == nil || ok {
			t.Errorf("dockerPublishedHostPort(%q) = (%q,%q,%v,%v), want an error", cmd, host, port, ok, err)
			continue
		}
		if !strings.Contains(err.Error(), spec) {
			t.Errorf("dockerPublishedHostPort(%q)'s error %q does not name the mapping %q", cmd, err, spec)
		}
	}
}

// A -p value is read as the shell keploy runs the command in hands it to
// docker: one pair of quotes off, and its variables expanded from keploy's
// environment, which the command runs with. Read as written, "$PORT:8080"
// named port "$PORT", and "${PORT:-8080}:8080" and a quoted mapping no port
// at all; an unset variable leaves the host port empty, which docker takes
// as one it picks.
func TestDockerPublishedHostPortReadsTheMappingAsTheShellHandsIt(t *testing.T) {
	t.Setenv("KEPLOY_ROW_PORT", "18080")
	t.Setenv("KEPLOY_ROW_UNSET", "")
	if err := os.Unsetenv("KEPLOY_ROW_UNSET"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ cmd, wantHost, wantPort string }{
		{"docker run -p $KEPLOY_ROW_PORT:8080 img", "127.0.0.1", "18080"},
		{"docker run -p ${KEPLOY_ROW_PORT}:8080 img", "127.0.0.1", "18080"},
		{`docker run -p "${KEPLOY_ROW_PORT}:8080" img`, "127.0.0.1", "18080"},
		{"docker run -p [::1]:${KEPLOY_ROW_PORT}:8080 img", "::1", "18080"},
		{"docker run -p ${KEPLOY_ROW_UNSET:-8081}:8080 img", "127.0.0.1", "8081"},
		{`docker run -p "8080:80" img`, "127.0.0.1", "8080"},
		{`docker run -p '8080:80' img`, "127.0.0.1", "8080"},
		{"docker run -p ${KEPLOY_ROW_UNSET}:8080 -p 9090:9090 img", "127.0.0.1", "9090"},
	} {
		host, port, ok, err := dockerPublishedHostPort(c.cmd)
		if err != nil || !ok || host != c.wantHost || port != c.wantPort {
			t.Errorf("dockerPublishedHostPort(%q) = (%q,%q,%v,%v), want (%q,%q,true,nil)", c.cmd, host, port, ok, err, c.wantHost, c.wantPort)
		}
	}
	if host, port, ok, err := dockerPublishedHostPort("docker run -p ${KEPLOY_ROW_UNSET}:8080 img"); ok || err != nil {
		t.Errorf("an unset host port = (%q,%q,%v,%v), want none to wait on and no error", host, port, ok, err)
	}
}

// listenOnAnIPv6Address listens where an app published on an IPv6 address
// would be, and returns that address. On [::1] where the host has an IPv6
// loopback; a host with IPv6 off has none, so there it listens on 127.0.0.1,
// addressed as ::ffff:127.0.0.1: the same address written as IPv6, with the
// colons that IPv6 has.
func listenOnAnIPv6Address(t *testing.T) (string, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Logf("no IPv6 loopback here (%v); addressing 127.0.0.1 as ::ffff:127.0.0.1", err)
		if ln, err = net.Listen("tcp4", "127.0.0.1:0"); err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return "::ffff:127.0.0.1", ln
	}
	t.Cleanup(func() { _ = ln.Close() })
	return "::1", ln
}

// A docker-run app published on an IPv6 address is waited for there, and the
// wait ends once the app accepts. Read on its colons, the mapping named
// another address, and for an app keploy runs the wait for it has no limit:
// the replay hung until the app exited or keploy was stopped. readyWithin
// bounds the row's wait.
func TestWaitForAppReady_DockerPortGate_IPv6HostIP(t *testing.T) {
	for _, form := range []struct{ name, spec string }{
		{"bracketed", "[%s]:%s:8080"},
		{"unbracketed", "%s:%s:8080"},
	} {
		t.Run(form.name, func(t *testing.T) {
			ip, ln := listenOnAnIPv6Address(t)
			_, port, _ := net.SplitHostPort(ln.Addr().String())
			cfg := &config.Config{Command: "docker run -p " + fmt.Sprintf(form.spec, ip, port) + " img"}
			cfg.Test.Delay = 0
			if !readyWithin(t, 10*time.Second, zap.NewNop(), cfg, httpProbeTarget{}) {
				t.Fatalf("waitForAppReady returned false for an app accepting at [%s]:%s", ip, port)
			}
		})
	}
}

// A mapping keploy cannot read a host address from is said as an error, and
// the first test waits on none of it rather than on an address no app
// listens at. The run goes on: there is nothing to wait on.
func TestWaitForAppReady_DockerPortGate_AnUnreadableMappingIsSaidNotWaitedOn(t *testing.T) {
	for _, spec := range []string{"[::1]:8080", "abc:8080"} {
		t.Run(spec, func(t *testing.T) {
			core, logs := observer.New(zap.ErrorLevel)
			cfg := &config.Config{Command: "docker run -p " + spec + " img"}
			cfg.Test.Delay = 0
			if !readyWithin(t, 10*time.Second, zap.New(core), cfg, httpProbeTarget{}) {
				t.Fatal("waitForAppReady returned false, which says the app exited or keploy was stopped")
			}
			said := unreadableSaid(logs)
			if len(said) != 1 {
				t.Fatalf("said %d errors about the mapping, want 1 (all: %v)", len(said), logs.All())
			}
			if got := fmt.Sprint(said[0].ContextMap()["error"]); !strings.Contains(got, spec) {
				t.Errorf("the error %q does not name the mapping %q", got, spec)
			}
		})
	}
}

// A docker-run app whose host port is already listening must return immediately
// (the gate adds no latency for a ready app).
func TestWaitForAppReady_DockerPortGate_ReadyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	cfg := &config.Config{Command: "docker run -p " + port + ":" + port + " img"}
	cfg.Test.Delay = 0 // no floor; isolate the port gate

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg, httpProbeTarget{}) {
		t.Fatal("waitForAppReady returned false for a listening port")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ready port should return promptly, took %v", elapsed)
	}
}

// A docker-run app whose host port never listens must still return true (proceed
// anyway, matching the historical fixed-delay behavior — the gate never blocks
// forever and never weakens the run) after the bounded ceiling.
func TestWaitForAppReady_DockerPortGate_DeadPortWaitsUntilTheAppExits(t *testing.T) {
	// A port nothing is listening on, held so no other socket takes it.
	_, port, _ := net.SplitHostPort(testnet.RefusedAddr(t, "tcp"))

	cfg := &config.Config{Command: "docker run -p " + port + ":" + port + " img"}
	cfg.Test.Delay = 0
	cfg.Test.HealthPollTimeout = 200 * time.Millisecond // no ceiling on the accept

	// The app's exit cancels its run's context: only that ends the wait.
	ctx, cancel := context.WithTimeout(pkg.EndsWithApp(context.Background()), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if waitForAppReady(ctx, zap.NewNop(), cfg, httpProbeTarget{}) {
		t.Fatal("waitForAppReady fired at a port that never accepted")
	}
	if elapsed := time.Since(start); elapsed < 1400*time.Millisecond {
		t.Fatalf("the wait ended after %v, before the app exited", elapsed)
	}
}

// ctx cancellation during the port wait must unblock immediately and report
// not-ready (false), preserving the "false only on ctx cancel" contract.
func TestWaitForAppReady_DockerPortGate_CtxCancel(t *testing.T) {
	_, port, _ := net.SplitHostPort(testnet.RefusedAddr(t, "tcp"))

	cfg := &config.Config{Command: "docker run -p " + port + ":" + port + " img"}
	cfg.Test.Delay = 0
	cfg.Test.HealthPollTimeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if waitForAppReady(ctx, zap.NewNop(), cfg, httpProbeTarget{}) {
		t.Fatal("waitForAppReady should return false when ctx is cancelled mid-wait")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ctx cancel should unblock promptly, took %v", elapsed)
	}
}

// The same for a compose app: its service's port entry keploy cannot read a
// host address from is said as an error, and not waited on.
func TestWaitForAppReady_ComposePortGate_AnUnreadableEntryIsSaidNotWaitedOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker-compose.yml")
	body := "services:\n  api:\n    container_name: app\n    ports: [\"[::1]:8080\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.ErrorLevel)
	cfg := &config.Config{Command: "docker compose -f " + path + " up", ContainerName: "app"}
	cfg.Test.Delay = 0
	if !readyWithin(t, 10*time.Second, zap.New(core), cfg, httpProbeTarget{}) {
		t.Fatal("waitForAppReady returned false, which says the app exited or keploy was stopped")
	}
	said := unreadableSaid(logs)
	if len(said) != 1 || said[0].ContextMap()["gate"] != "compose-published-port" {
		t.Fatalf("said %v, want one error from the compose gate", logs.All())
	}
}

// unreadableSaid is the errors a gate said about a mapping it could not read:
// an error naming its gate, whatever its words.
func unreadableSaid(logs *observer.ObservedLogs) []observer.LoggedEntry {
	return logs.FilterLevelExact(zap.ErrorLevel).FilterFieldKey("gate").All()
}

// A -p host port given as a variable is waited on where the shell sends
// docker: the app's port. Read as written it was port "$PORT", and the wait
// for it never ended; read as unreadable it was not waited on at all.
func TestWaitForAppReady_DockerPortGate_AVariablePortIsWaitedOn(t *testing.T) {
	t.Run("accepting", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		t.Setenv("KEPLOY_ROW_PORT", port)
		core, logs := observer.New(zap.ErrorLevel)
		cfg := &config.Config{Command: "docker run -p $KEPLOY_ROW_PORT:8080 img"}
		cfg.Test.Delay = 0
		if !readyWithin(t, 10*time.Second, zap.New(core), cfg, httpProbeTarget{}) {
			t.Fatal("waitForAppReady returned false for an app accepting on the variable's port")
		}
		if said := unreadableSaid(logs); len(said) != 0 {
			t.Fatalf("said the mapping cannot be read: %v", said)
		}
	})
	t.Run("not yet accepting", func(t *testing.T) {
		_, port, _ := net.SplitHostPort(testnet.RefusedAddr(t, "tcp"))
		t.Setenv("KEPLOY_ROW_PORT", port)
		cfg := &config.Config{Command: "docker run -p ${KEPLOY_ROW_PORT}:8080 img"}
		cfg.Test.Delay = 0
		// The app's exit cancels its run's context: only that ends the wait.
		ctx, cancel := context.WithTimeout(pkg.EndsWithApp(context.Background()), 1500*time.Millisecond)
		defer cancel()
		if waitForAppReady(ctx, zap.NewNop(), cfg, httpProbeTarget{}) {
			t.Fatal("waitForAppReady fired at the variable's port, which never accepted")
		}
	})
}

// A compose app's ports are read as compose reads them: variables expanded
// from the environment and the project's .env, and a long-form entry by the
// short form's rules. Read as written, a long-form "${PORT}", 0 or "abc" was
// a port to wait on, where no wait ever ended, and a short "${PORT:-8080}"
// was said to be unreadable.
func TestWaitForAppReady_ComposePortGate_ReadsThePortsAsComposeDoes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	t.Setenv("KEPLOY_ROW_PORT", port)
	t.Setenv("KEPLOY_ROW_UNSET", "")
	if err := os.Unsetenv("KEPLOY_ROW_UNSET"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, ports string
		said        int
	}{
		{"long form, a set variable", "\n      - target: 8080\n        published: \"${KEPLOY_ROW_PORT}\"", 0},
		{"long form, an unset variable", "\n      - target: 8080\n        published: \"${KEPLOY_ROW_UNSET}\"", 0},
		{"long form, port 0", "\n      - target: 8080\n        published: 0", 0},
		{"long form, no port", "\n      - target: 8080\n        published: abc", 1},
		{"long form, out of range", "\n      - target: 8080\n        published: 70000", 1},
		{"short form, a default", " [\"${KEPLOY_ROW_UNSET:-" + port + "}:8080\"]", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docker-compose.yml")
			body := "services:\n  api:\n    container_name: app\n    ports:" + c.ports + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			core, logs := observer.New(zap.ErrorLevel)
			cfg := &config.Config{Command: "docker compose -f " + path + " up", ContainerName: "app"}
			cfg.Test.Delay = 0
			if !readyWithin(t, 10*time.Second, zap.New(core), cfg, httpProbeTarget{}) {
				t.Fatal("waitForAppReady returned false, which says the app exited or keploy was stopped")
			}
			if said := unreadableSaid(logs); len(said) != c.said {
				t.Fatalf("said %d errors about the entry, want %d: %v", len(said), c.said, said)
			}
		})
	}
}
