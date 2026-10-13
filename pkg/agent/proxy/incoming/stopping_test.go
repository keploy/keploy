package proxy

import (
	"context"
	"net"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// refusedAddr is an address nothing listens on: a dial to it is refused.
func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// dialGoneApp has the ingress take a connection for an app that refuses it,
// and returns the levels it logged the failed dial at.
func dialGoneApp(t *testing.T, pm *IngressProxyManager, logs *observer.ObservedLogs, app string) []zapcore.Level {
	t.Helper()
	// A done ctx ends the dial's retries at its first refusal; the dial and
	// what the ingress logs of it are what they are otherwise.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client, peer := net.Pipe()
	defer peer.Close()
	defer client.Close()
	pm.handleHttp1Connection(ctx, client, app, pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8080)
	var got []zapcore.Level
	for _, e := range logs.TakeAll() {
		if e.Message == "Failed to connect to upstream application. Verify the application is listening on the resolved address." {
			got = append(got, e.Level)
		}
	}
	return got
}

// While the recording is stopping, its client has stopped the app on purpose:
// a connection the app refuses then is said at DEBUG. Before the stop, and
// once a new recording has begun, the same refusal is an ERROR.
func TestIngressSaysARefusedAppAtDebugOnlyWhileStopping(t *testing.T) {
	app := refusedAddr(t)
	core, logs := observer.New(zapcore.DebugLevel)
	pm := New(zap.New(core), nil, &config.Config{})

	for _, step := range []struct {
		stopping bool
		want     zapcore.Level
	}{
		{false, zapcore.ErrorLevel},
		{true, zapcore.DebugLevel},
		{false, zapcore.ErrorLevel},
	} {
		pm.SetStopping(step.stopping)
		got := dialGoneApp(t, pm, logs, app)
		if len(got) != 1 || got[0] != step.want {
			t.Fatalf("stopping=%v: the refused dial was logged at %v, want once at %v", step.stopping, got, step.want)
		}
	}
}

// Only a refusal is the app being gone on purpose. Any other failure to reach
// it is an ERROR while stopping too: the agent is still working through what
// it captured before the stop.
func TestIngressSaysOtherAppFailuresAtErrorWhileStopping(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	pm := New(zap.New(core), nil, &config.Config{})
	pm.SetStopping(true)
	pm.logAppDialFailure(pm.logger, "Failed to connect to upstream application.", context.DeadlineExceeded)
	if all := logs.All(); len(all) != 1 || all[0].Level != zapcore.ErrorLevel {
		t.Fatalf("a dial that timed out while stopping was logged as %v, want one ERROR", all)
	}
}

// A logger that writes nothing below INFO writes nothing of a refused dial
// while stopping.
func TestIngressRefusedAppIsQuietWithoutDebugWhileStopping(t *testing.T) {
	app := refusedAddr(t)
	core, logs := observer.New(zapcore.InfoLevel)
	pm := New(zap.New(core), nil, &config.Config{})
	pm.SetStopping(true)
	if got := dialGoneApp(t, pm, logs, app); len(got) != 0 {
		t.Fatalf("logged the refused dial at %v while stopping, at INFO", got)
	}
}
