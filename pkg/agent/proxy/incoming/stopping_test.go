package proxy

import (
	"context"
	"errors"
	"net"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// dialGoneApp has the ingress take a connection for an app that has gone: it
// listened at app, and refuses it now. It returns the levels the ingress
// logged the failed dial at.
func dialGoneApp(t *testing.T, pm *IngressProxyManager, logs *observer.ObservedLogs, app string) []zapcore.Level {
	t.Helper()
	// What a dial that connected to it left (dialApp): its refusal is then
	// final, and the dial returns it at once, with nothing to end it but a
	// context that does not end.
	pm.listened.Store(app, struct{}{})
	client, peer := net.Pipe()
	defer peer.Close()
	defer client.Close()
	pm.handleHttp1Connection(context.Background(), client, app, pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8080)
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
	app := testnet.RefusedAddr(t, "tcp")
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

// A dial keploy itself ended (the forwarder's or the agent's context
// canceled while it dialed) is not the app failing: it is said at DEBUG,
// whether or not the recording is stopping.
func TestIngressSaysADialKeployEndedAtDebug(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(zap.NewNop(), nil, &config.Config{}).dialApp(ctx, testnet.RefusedAddr(t, "tcp"))
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
