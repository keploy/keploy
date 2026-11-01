package proxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// dialGoneApp has the ingress take a connection for an app that refuses it,
// as one the recording's stop took does, while the recording is stopping.
// It returns the levels the ingress logged the failed dial at. The refusal is
// final while stopping, and comes back at once: a dial that waited on it
// instead fails the test once its bounded context ends, not by hanging.
func dialGoneApp(t *testing.T, pm *IngressProxyManager, logs *observer.ObservedLogs, app string) []zapcore.Level {
	t.Helper()
	client, peer := net.Pipe()
	defer peer.Close()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pm.handleHttp1Connection(ctx, client, app, pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8080)
	if ctx.Err() != nil {
		t.Fatal("the ingress waited on a refusal while stopping until its context ended")
	}
	var got []zapcore.Level
	for _, e := range logs.TakeAll() {
		if e.Message == "Failed to connect to upstream application. Verify the application is listening on the resolved address." {
			got = append(got, e.Level)
		}
	}
	return got
}

// While the recording is stopping, its client has stopped the app on purpose:
// a connection the app refuses then is said at DEBUG, at once, in every stop.
// (Before the stop, and once a new recording has begun, the same refusal is
// the app not listening yet, or again: the call waits for its listener,
// appdial_test.go.)
func TestIngressSaysARefusedAppAtDebugWhileStopping(t *testing.T) {
	app := testnet.RefusedAddr(t, "tcp")
	core, logs := observer.New(zapcore.DebugLevel)
	pm := New(zap.New(core), nil, &config.Config{})
	for stop := 0; stop < 2; stop++ {
		pm.SetStopping(true)
		got := dialGoneApp(t, pm, logs, app)
		if len(got) != 1 || got[0] != zapcore.DebugLevel {
			t.Fatalf("stop %d: the refused dial was logged at %v, want once at DEBUG", stop, got)
		}
		pm.SetStopping(false) // a new recording begins
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

// A dial keploy itself ended (the forwarder's or the agent's context
// canceled while it dialed) is not the app failing: it is said at DEBUG,
// whether or not the recording is stopping.
func TestIngressSaysADialKeployEndedAtDebug(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(zap.NewNop(), nil, &config.Config{}).dialApp(ctx, testnet.RefusedAddr(t, "tcp"), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a dial under a canceled context returned %v, want the context's end", err)
	}
	for _, stopping := range []bool{false, true} {
		core, logs := observer.New(zapcore.DebugLevel)
		pm := New(zap.New(core), nil, &config.Config{})
		pm.SetStopping(stopping)
		pm.logAppDialFailure(pm.logger, "Failed to connect to upstream application.", err)
		if all := logs.All(); len(all) != 1 || all[0].Level != zapcore.DebugLevel {
			t.Fatalf("stopping=%v: a dial keploy ended was logged as %v, want one DEBUG", stopping, all)
		}
	}
}

// A logger that writes nothing below INFO writes nothing of a refused dial
// while stopping.
func TestIngressRefusedAppIsQuietWithoutDebugWhileStopping(t *testing.T) {
	app := testnet.RefusedAddr(t, "tcp")
	core, logs := observer.New(zapcore.InfoLevel)
	pm := New(zap.New(core), nil, &config.Config{})
	pm.SetStopping(true)
	if got := dialGoneApp(t, pm, logs, app); len(got) != 0 {
		t.Fatalf("logged the refused dial at %v while stopping, at INFO", got)
	}
}
