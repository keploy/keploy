package replay

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, port, ok := dockerPublishedHostPort(c.cmd)
			if ok != c.wantOK || host != c.wantHost || port != c.wantPort {
				t.Fatalf("dockerPublishedHostPort(%q) = (%q,%q,%v), want (%q,%q,%v)",
					c.cmd, host, port, ok, c.wantHost, c.wantPort, c.wantOK)
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
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg) {
		t.Fatal("waitForAppReady returned false for a listening port")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ready port should return promptly, took %v", elapsed)
	}
}

// A docker-run app whose host port never listens must still return true (proceed
// anyway, matching the historical fixed-delay behavior — the gate never blocks
// forever and never weakens the run) after the bounded ceiling.
func TestWaitForAppReady_DockerPortGate_DeadPortProceeds(t *testing.T) {
	// Pick a port nothing is listening on by opening then closing a listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()

	cfg := &config.Config{Command: "docker run -p " + port + ":" + port + " img"}
	cfg.Test.Delay = 0
	cfg.Test.HealthPollTimeout = 1 * time.Second // tiny ceiling for the test

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg) {
		t.Fatal("waitForAppReady should proceed (return true) after the ceiling on a dead port")
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("should have waited ~the ceiling before proceeding, took only %v", elapsed)
	}
}

// ctx cancellation during the port wait must unblock immediately and report
// not-ready (false), preserving the "false only on ctx cancel" contract.
func TestWaitForAppReady_DockerPortGate_CtxCancel(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()

	cfg := &config.Config{Command: "docker run -p " + port + ":" + port + " img"}
	cfg.Test.Delay = 0
	cfg.Test.HealthPollTimeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if waitForAppReady(ctx, zap.NewNop(), cfg) {
		t.Fatal("waitForAppReady should return false when ctx is cancelled mid-wait")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ctx cancel should unblock promptly, took %v", elapsed)
	}
}
