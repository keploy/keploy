package replay

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The replay target's host name in keploy's log is its labels' shape beside
// its tag (models.HostLogName), never the name: a host name is traffic (an
// app's own name, a tenant's subdomain, a Service name), and a program that
// embeds the replay (k8s-proxy) writes these lines into its own log. The
// console and the report name it whole.
//
// These rows guard that by value, not by how a line is written: each drives
// one place that logs the target's host with a made-up name (canaryHost) and
// fails if any line, at any level, holds it in any form, in its message or in
// any field of any type or key (logleak.Check: a string, an error, a value
// zap encodes, a Stringer). A host put back whole by any spelling fails its
// row.

// canaryHost is a made-up host name for the replay target, where an app's own
// name, a tenant's subdomain or a Service name would be: under .invalid
// (RFC 6761), so no lookup of it answers, and holding logleak.Canary.
const canaryHost = "app-" + logleak.Canary + ".invalid"

// canaryRecordedHost is a made-up host name for the Host header a test was
// recorded with.
const canaryRecordedHost = "rec-" + logleak.Canary + ".invalid"

// canaryHealthHost is a made-up host name for a --health-url's host, apart
// from the address the set's first test dials.
const canaryHealthHost = "health-" + logleak.Canary + ".invalid"

// unpublished is an address reach that says the host can reach no address:
// Docker mode with the app's port not published.
type unpublished struct{}

func (unpublished) UnreachableAppPort(context.Context, string, uint16, uint16) string {
	return "it is not published on the host"
}

// lineSaid waits until logs holds a line whose message holds snippet.
func lineSaid(t *testing.T, logs *observer.ObservedLogs, snippet string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for logs.FilterMessageSnippet(snippet).Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("precondition: no line said %q within 30s: %v", snippet, logs.All())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// said fails t unless logs holds a line whose message holds snippet: the row
// drove the place it names.
func said(t *testing.T, logs *observer.ObservedLogs, snippet string) {
	t.Helper()
	if logs.FilterMessageSnippet(snippet).Len() == 0 {
		t.Fatalf("precondition: no line said %q: %v", snippet, logs.All())
	}
}

// acceptingAddr is a loopback address that accepts a connection (its
// backlog), for a gate's TCP stage to pass, closed when the test ends.
func acceptingAddr(t *testing.T) (host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	host, port, _ = net.SplitHostPort(ln.Addr().String())
	return host, port
}

// untilLine runs wait in a goroutine on a context it cancels once logs holds
// a line whose message holds snippet, and returns once wait has.
func untilLine(t *testing.T, ctx context.Context, logs *observer.ObservedLogs, snippet string, wait func(context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait(ctx)
	}()
	lineSaid(t, logs, snippet)
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the wait did not end once its context did")
	}
}

func TestNoLogLineNamesTheReplayTargetsHostWhole(t *testing.T) {
	target := net.JoinHostPort(canaryHost, "8080")
	for _, row := range []struct {
		name string
		run  func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs)
	}{
		{"the gate on an address that never accepts warns and fires the tests", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			cfg := &config.Config{}
			cfg.Test.HealthPollTimeout = time.Second
			if !gateOnAppAddress(context.Background(), logger, cfg, canaryHost, "8080", "app-ready-probe-addr", httpProbeTarget{}) {
				t.Fatal("precondition: the gate did not fire the tests")
			}
			said(t, logs, "app address did not accept a connection within the ceiling")
		}},
		{"the gate's wait for an application keploy started says what it waits on", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			untilLine(t, pkg.EndsWithApp(context.Background()), logs, "waiting for the application to accept a connection on", func(ctx context.Context) {
				gateOnAppAddress(ctx, logger, &config.Config{}, canaryHost, "8080", "resolved-test-target", httpProbeTarget{})
			})
		}},
		{"the gate's HTTP stage moves on from a test's address the host cannot reach", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			defer func(first, most time.Duration) { probeReachRecheckFirst, probeReachRecheckMax = first, most }(probeReachRecheckFirst, probeReachRecheckMax)
			probeReachRecheckFirst, probeReachRecheckMax = time.Millisecond, time.Millisecond
			host, port := acceptingAddr(t)
			cfg := &config.Config{}
			cfg.Test.HealthPollTimeout = 20 * time.Second
			src := &probeTargetSource{logger: logger, reach: unpublished{}, seen: map[string]bool{}}
			gateOnAppAddress(context.Background(), logger, cfg, host, port, "docker-published-port",
				httpProbeTarget{scheme: "http", host: canaryHost, port: "8080", appPort: 8080, ok: true, src: src})
			said(t, logs, "readiness gate: the host cannot reach the probed test's address")
		}},
		{"the gate's HTTP stage that never completes a round-trip warns", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			host, port := acceptingAddr(t)
			cfg := &config.Config{}
			cfg.Test.HealthPollTimeout = time.Second
			gateOnAppAddress(context.Background(), logger, cfg, host, port, "docker-published-port",
				httpProbeTarget{scheme: "http", host: canaryHost, port: "8080", ok: true})
			said(t, logs, "app accepted TCP but never completed an HTTP round-trip")
		}},
		{"the gate's HTTP stage that completes a round-trip says so", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			reportHTTPGate(context.Background(), logger, "docker-published-port", &config.Config{}, time.Second,
				httpProbeTarget{scheme: "http", host: canaryHost, port: "8080", ok: true}, nil)
			said(t, logs, "app readiness confirmed by a completed HTTP round-trip")
		}},
		{"the gate skips a test whose address the host cannot reach", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			testCfg := config.Test{Host: canaryHost}
			if p := resolveTestSetProbeTarget(context.Background(), testCfg, httpCase("http://"+target+"/orders"), "test-set-0", logger, unpublished{}, nil); p.ok {
				t.Fatalf("precondition: the gate probes %+v", p)
			}
			said(t, logs, "readiness gate: not probing a test the host cannot reach")
		}},
		{"a health URL that never answers 2xx is polled, then the fixed delay and its address", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			cfg := &config.Config{}
			cfg.Test.HealthURL = "http://" + target + "/health"
			cfg.Test.HealthPollTimeout = time.Second
			waitForAppReady(context.Background(), logger, cfg, httpProbeTarget{})
			said(t, logs, "polling application health endpoint before firing tests")
			said(t, logs, "health probe timed out, falling back to fixed delay")
		}},
		{"a health URL that answers 2xx", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			was := healthPoller
			defer func() { healthPoller = was }()
			healthPoller = func(context.Context, string) bool { return true }
			cfg := &config.Config{}
			cfg.Test.HealthURL = "http://" + target + "/health"
			if !waitForAppReady(context.Background(), logger, cfg, httpProbeTarget{}) {
				t.Fatal("precondition: the wait did not end ready")
			}
			said(t, logs, "health endpoint reported 2xx; proceeding")
		}},
		{"a health URL that never answers 2xx, then the wait for its address to accept", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			cfg := &config.Config{}
			cfg.Test.HealthURL = "http://" + target + "/health"
			cfg.Test.HealthPollTimeout = time.Second
			untilLine(t, pkg.EndsWithApp(context.Background()), logs, "waiting for the application to accept a connection on", func(ctx context.Context) {
				waitForAppReady(ctx, logger, cfg, httpProbeTarget{})
			})
			said(t, logs, "the first test waits instead for the application to accept a connection on its port")
		}},
		{"a health URL waited on with probing the address disabled", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			was := sayStillWaiting
			defer func() { sayStillWaiting = was }()
			// The line is pkg.SayStillWaiting's (its own row names the
			// address by its shape): it is said every 30s, so the row
			// takes what it is asked to say and logs it as that line does.
			sayStillWaiting = func(_ context.Context, logger *zap.Logger, what, _ string, _ time.Time) func() {
				logger.Info("still waiting for the application to " + what)
				return func() {}
			}
			cfg := &config.Config{}
			cfg.Test.HealthURL = "http://" + target + "/health"
			cfg.Test.HealthPollTimeout = time.Second
			cfg.Test.DisableAppReadyProbe = true
			untilLine(t, pkg.EndsWithApp(context.Background()), logs, "still waiting for the application to answer 2xx at", func(ctx context.Context) {
				waitForAppReady(ctx, logger, cfg, httpProbeTarget{})
			})
			said(t, logs, "probing the app's address is disabled by configuration")
		}},
		{"an invalid --health-url", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			cfg := &config.Config{}
			cfg.Test.HealthURL = "http://" + canaryHost + ":80:80/health"
			waitForAppReady(context.Background(), logger, cfg, httpProbeTarget{})
			said(t, logs, "invalid --health-url")
		}},
		{"an invalid test.appReadyProbeAddr", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			cfg := &config.Config{}
			cfg.Test.AppReadyProbeAddr = canaryHost
			gateOnAppAddresses(context.Background(), logger, cfg, httpProbeTarget{}, "")
			said(t, logs, "invalid test.appReadyProbeAddr")
		}},
		{"the proven wait with an agent that cannot watch, the first test's address deciding", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			manualWake(t)
			ctx := models.StartLogKeySession(context.Background())
			a := &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{fmt.Errorf("%w: 404", models.ErrReadinessUnsupported)}}
			p := provenReadiness{logger: logger, keys: models.LogKeys(ctx), cfg: testCfg(), agent: a, host: canaryHost, port: "8080", listen: 8080}
			untilLine(t, ctx, logs, "the first test's address taking a connect decides", func(ctx context.Context) { p.wait(ctx) })
		}},
		{"the proven wait with an agent that cannot watch, the health URL deciding", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			manualWake(t)
			ctx := models.StartLogKeySession(context.Background())
			cfg := testCfg()
			cfg.Test.HealthURL = "http://" + target + "/up"
			a := &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{fmt.Errorf("%w: 404", models.ErrReadinessUnsupported)}}
			p := provenReadiness{logger: logger, keys: models.LogKeys(ctx), cfg: cfg, agent: a, host: canaryHost, port: "8080", listen: 8080}
			untilLine(t, ctx, logs, "the health URL alone decides", func(ctx context.Context) { p.wait(ctx) })
		}},
		{"the proven wait's still-waiting line, for each thing it waits on", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			ctx := models.StartLogKeySession(context.Background())
			// What its last probe said: a dial's error, which names the
			// host it looked up.
			_, lastProbe := takesConnect(ctx, target)
			if !logleak.Holds(lastProbe, logleak.Canary) {
				t.Fatalf("precondition: the probe's text %q does not name the host", lastProbe)
			}
			health := "http://" + net.JoinHostPort(canaryHealthHost, "8080") + "/up"
			_, lastHealth := answers(ctx, health, false)
			if !strings.Contains(lastHealth, canaryHealthHost) {
				t.Fatalf("precondition: the health probe's text %q does not name its host", lastHealth)
			}
			base := provenReadiness{logger: logger, keys: models.LogKeys(ctx), cfg: testCfg(), host: canaryHost, port: "8080", listen: 8080}
			agent := &fakeAgent{answers: []models.AppReadiness{{}}}
			for _, w := range []struct {
				health, lastProbe string
				p                 func(provenReadiness) provenReadiness
			}{
				{"", "", func(p provenReadiness) provenReadiness { p.agent = agent; return p }},
				{"", "", func(p provenReadiness) provenReadiness {
					p.agent, p.listen, p.resolve = agent, 0, func(context.Context) uint16 { return 0 }
					return p
				}},
				{health, lastHealth, func(p provenReadiness) provenReadiness { return p }},
				{"", lastProbe, func(p provenReadiness) provenReadiness { p.dial = true; return p }},
			} {
				w.p(base).sayStillWaiting(w.health, w.lastProbe, "", models.AppReadiness{}, progressMark{}, progressMark{}, 1, 0)
			}
			if n := logs.FilterMessageSnippet("still waiting for the application before the set's first test").Len(); n != 4 {
				t.Fatalf("precondition: the line was said %d times, want 4: %v", n, logs.All())
			}
		}},
		{"the proven wait of a keploy that cannot ask its agent", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			t.Setenv(provenswitch.Env, "1")
			manualWake(t)
			cfg := testCfg()
			cfg.Test.Host = canaryHost
			cfg.Test.HealthURL = "http://" + canaryHost + ":80:80/up"
			r := &Replayer{logger: logger, config: cfg, instrumentation: struct{ Instrumentation }{}}
			untilLine(t, context.Background(), logs, "the first test's address taking a connect decides", func(ctx context.Context) {
				r.appReady(ctx, httpCase("http://recorded.example:8080/orders"), "test-set-0", true, false)
			})
			said(t, logs, "invalid --health-url")
		}},
		{"a base path put in a test's URL", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			cfg := &config.Config{}
			cfg.Test.BasePath = "http://" + target + "/api"
			r := &Replayer{logger: logger, config: cfg}
			tc := &models.TestCase{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://recorded.example:8080/orders"}}
			if err := r.applyBasePath(context.Background(), tc); err != nil {
				t.Fatal(err)
			}
			said(t, logs, "test case request origin")
		}},
		{"a reset test request waits for the app keploy started to answer", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			cfg := &config.Config{Command: "node app.js"}
			cfg.Test.Host = canaryHost
			r := &Replayer{config: cfg, logger: logger}
			var console bytes.Buffer
			r.SetConsole(&console)
			tc := &models.TestCase{Name: "test-1", Kind: models.HTTP, AppPort: 8080, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://recorded.example:8080/orders"}}
			untilLine(t, pkg.EndsWithApp(context.Background()), logs, "the test request was reset at", func(ctx context.Context) {
				resetSaid := false
				r.waitForResetResendReady(ctx, tc, "test-set-0", &resetSaid)
			})
			said(t, logs, "app not serving HTTP before reset re-send")
			if !strings.Contains(console.String(), canaryHost) {
				t.Errorf("the console does not name the target whole: %q", console.String())
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			logger, logs := logleak.Observe()
			row.run(t, logger, logs)
			logleak.Check(t, logs, logleak.Canary, row.name)
		})
	}
}

// A base-path run against an app named by a host name logs the name by its
// shape and tag only: the base path, the Host header each test was recorded
// with, and every line of a test that could not be sent there (its name does
// not resolve), with the proven switch off and on. The report keeps the
// test's request whole.
func TestABasePathRunNamesTheTargetsHostByItsShapeOnly(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			recorded := net.JoinHostPort(canaryRecordedHost, strconv.Itoa(int(closedLocalPort(t))))
			tc := bpRequest("test-1", 8080, "/orders")
			tc.HTTPReq.URL = "http://" + recorded + "/orders"
			tc.HTTPReq.Header["Host"] = recorded
			run := newBasePathRunOf(t, "http://"+canaryHost+":8080/api", nil, tc)
			logs := debugRun(run)
			_, _ = run.start(t)
			said(t, logs, "failed to send testcase request to app")
			said(t, logs, "Keploy will not mock the outgoing calls when base path is provided")
			said(t, logs, "not under the Host header they were recorded with")
			logleak.Check(t, logs, logleak.Canary, "a base-path run against "+canaryHost)
		})
	}
}

// Under the proven switch a set whose health URL names a host that never
// answers is not judged: the reason keeps the URL whole for the report, and
// its ERROR line, as every other line of the run, names the host by its shape
// and tag.
func TestASetNeverReadyAtAHealthURLsHostNamesItByItsShapeOnly(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	wake := manualWake(t)
	stallLimit(t, 2)
	app := newBPApp(t)
	run := newBasePathRun(t, app.URL, "test-1")
	run.replayer.config.Test.HealthURL = "http://" + canaryHost + ":8080/up"
	logs := debugRun(run)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case wake <- struct{}{}:
				time.Sleep(10 * time.Millisecond)
			case <-done:
				return
			}
		}
	}()
	_, _ = run.start(t)
	if n := logs.FilterMessageSnippet("its application never got ready").Len(); n != 1 {
		t.Fatalf("precondition: the set's not-judged line was logged %d times: %v", n, logs.All())
	}
	logleak.Check(t, logs, logleak.Canary, "a set whose health URL names "+canaryHost)
	if rep := run.report.report; rep == nil || !strings.Contains(rep.FailureReason, canaryHost) {
		t.Errorf("the report's reason does not name the health URL's host whole: %+v", rep)
	}
}

// debugRun is run's replayer built again with a logger that keeps every line
// at debug level (logleak.Observe), and those lines.
func debugRun(run *bpRun) *observer.ObservedLogs {
	logger, logs := logleak.Observe()
	svc := NewReplayer(logger, run.tests, run.mocks, run.report, run.mappings,
		bpTestSetConf{}, prTelemetry{}, run.agent, nil, run.replayer.config)
	run.replayer = svc.(*Replayer)
	run.logs = logs
	return logs
}
