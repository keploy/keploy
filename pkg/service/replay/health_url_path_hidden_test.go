package replay

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// hiddenSecret is what a readiness path that is itself a secret holds in
// these rows: in its path, its query and its fragment.
const hiddenSecret = "s3cret-Rp7q"

// withHiddenPath is base with a path, query and fragment that hold
// hiddenSecret.
func withHiddenPath(base string) string {
	return base + "/" + hiddenSecret + "/ready?tok=" + hiddenSecret + "#" + hiddenSecret
}

// namedAs is how a line names withHiddenPath(base): by its scheme and host
// only with its path hidden; off, as before: its path kept, its query and
// fragment dropped.
func namedAs(base string, hide bool) string {
	if hide {
		return base + "/<path-redacted>"
	}
	return base + "/" + hiddenSecret + "/ready"
}

// hiddenCfg is a replay config whose health URL is withHiddenPath(base), with
// its path hidden from every line when hide.
func hiddenCfg(base string, pollTimeout time.Duration, hide bool) *config.Config {
	cfg := newCfg(withHiddenPath(base), pollTimeout)
	cfg.Test.Delay = 0
	cfg.Test.HideHealthURLPath = hide
	return cfg
}

// eachWay runs row with the path hidden, then with it not hidden.
func eachWay(t *testing.T, row func(t *testing.T, hide bool)) {
	t.Helper()
	t.Run("hidden", func(t *testing.T) { row(t, true) })
	t.Run("not hidden", func(t *testing.T) { row(t, false) })
}

// saidNowhere fails the row when any line logs holds secret, in its message
// or in any of its fields.
func saidNowhere(t *testing.T, logs *observer.ObservedLogs, secret string) {
	t.Helper()
	for _, e := range logs.All() {
		if strings.Contains(e.Message, secret) {
			t.Errorf("%s line %q holds %q", e.Level, e.Message, secret)
		}
		for k, v := range e.ContextMap() {
			if s := fmt.Sprint(v); strings.Contains(s, secret) {
				t.Errorf("%s line %q: its %s %q holds %q", e.Level, e.Message, k, s, secret)
			}
		}
	}
}

// keptOut fails the row when logs hold what hide keeps out of them: with the
// path hidden, any part of it; off, the query and fragment the lines have
// always dropped.
func keptOut(t *testing.T, logs *observer.ObservedLogs, hide bool) {
	t.Helper()
	if hide {
		saidNowhere(t, logs, hiddenSecret)
		return
	}
	saidNowhere(t, logs, "tok=")
	saidNowhere(t, logs, "#"+hiddenSecret)
}

// saidWith returns the healthUrl field of the one line whose message holds
// snippet, failing the row when there is none.
func saidWith(t *testing.T, logs *observer.ObservedLogs, snippet string) string {
	t.Helper()
	return saidField(t, logs, snippet, "healthUrl")
}

// saidField returns field of the first line whose message holds snippet,
// failing the row when there is none.
func saidField(t *testing.T, logs *observer.ObservedLogs, snippet, field string) string {
	t.Helper()
	said := logs.FilterMessageSnippet(snippet).All()
	if len(said) == 0 {
		t.Fatalf("no line says %q; lines: %v", snippet, logs.All())
	}
	return fmt.Sprint(said[0].ContextMap()[field])
}

// With the path hidden, a health URL is named by its scheme and host only,
// whatever else it holds; a URL with no host by a placeholder, its text being
// all path; and an unparseable one, by a reason that quotes no part of it. A
// configured health path is said as redacted, keploy's own as it is. Off,
// each is named as before: the URL's path kept, its user, query and fragment
// dropped, and the health path said whole.
func TestAHiddenHealthURLPathIsNamedByItsSchemeAndHostOnly(t *testing.T) {
	for raw, want := range map[string]string{
		"http://localhost:8080/" + hiddenSecret + "?tok=" + hiddenSecret + "#" + hiddenSecret: "http://localhost:8080/<path-redacted>",
		"https://u:" + hiddenSecret + "@10.0.0.5:8443/" + hiddenSecret:                        "https://10.0.0.5:8443/<path-redacted>",
		"http://localhost:8080":                         "http://localhost:8080/<path-redacted>",
		"//10.0.0.5:8080/" + hiddenSecret:               "//10.0.0.5:8080/<path-redacted>",
		"http://[::1]:8080/" + hiddenSecret:             "http://[::1]:8080/<path-redacted>",
		"http://:8080/" + hiddenSecret:                  "http://:8080/<path-redacted>",
		"localhost:8080/" + hiddenSecret:                "<health-url-redacted>",
		"/" + hiddenSecret:                              "<health-url-redacted>",
		"http://localhost:8080/" + hiddenSecret + "%zz": "<unparseable-health-url-redacted>",
	} {
		if got := sanitizeHealthURL(raw, true); got != want {
			t.Errorf("sanitizeHealthURL(%q, hidden) = %q, want %q", raw, got, want)
		}
	}
	if got, want := sanitizeHealthURL("http://u:pw@localhost:8080/"+hiddenSecret+"?tok=1#f", false), "http://localhost:8080/"+hiddenSecret; got != want {
		t.Errorf("sanitizeHealthURL(not hidden) = %q, want %q: the path kept, as before", got, want)
	}
	if got := healthURLWaitAddr("/"+hiddenSecret, true); got != "<health-url-redacted>" {
		t.Errorf("healthURLWaitAddr(no host, hidden) = %q, want the placeholder", got)
	}
	if got := healthURLWaitAddr("/"+hiddenSecret, false); got != "/"+hiddenSecret {
		t.Errorf("healthURLWaitAddr(no host, not hidden) = %q, want the URL as given, as before", got)
	}
	bad := "http://localhost:8080/" + hiddenSecret + "%zz"
	if reason, ok := validateHealthURL(bad, true); ok || reason != "it does not parse as a URL" {
		t.Errorf("validateHealthURL(%q, hidden) = %q, %v; want refused with a reason that quotes no part of it", bad, reason, ok)
	}
	if reason, ok := validateHealthURL(bad, false); ok || !strings.Contains(reason, hiddenSecret) {
		t.Errorf("validateHealthURL(%q, not hidden) = %q, %v; want the parser's reason, as before", bad, reason, ok)
	}
	for _, c := range []struct {
		configured string
		hide       bool
		want       string
	}{
		{"/" + hiddenSecret, true, "/<path-redacted>"},
		{hiddenSecret, true, "/<path-redacted>"},
		{"", true, keployReadinessProbePath},
		{hiddenSecret, false, "/" + hiddenSecret},
		{"", false, keployReadinessProbePath},
	} {
		if got := healthProbePathForLog(c.configured, c.hide); got != c.want {
			t.Errorf("healthProbePathForLog(%q, hidden %v) = %q, want %q", c.configured, c.hide, got, c.want)
		}
	}
}

// A probe of a health URL says what answered by its scheme and host only
// with the path hidden, as before when not: a 2xx, a server whose certificate
// keploy does not trust, a URL that does not parse, and a redirect whose
// Location echoes the path and does not parse.
func TestAProbeOfAHiddenHealthURLSaysItsSchemeAndHostOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer tlsSrv.Close()
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", r.URL.Path+"%zz")
		w.WriteHeader(http.StatusFound)
	}))
	defer echo.Close()
	ctx := context.Background()
	eachWay(t, func(t *testing.T, hide bool) {
		if ok, what := answers(ctx, withHiddenPath(srv.URL), false, hide); !ok || what != namedAs(srv.URL, hide)+" answered 200" {
			t.Errorf("answers(2xx) = %v, %q, want %q", ok, what, namedAs(srv.URL, hide)+" answered 200")
		}
		if ok, what := answers(ctx, withHiddenPath(tlsSrv.URL), true, hide); !ok || what != namedAs(tlsSrv.URL, hide)+" answered (a certificate keploy does not trust)" {
			t.Errorf("answers(untrusted certificate) = %v, %q", ok, what)
		}
		ok, what := answers(ctx, withHiddenPath(echo.URL), false, hide)
		if ok || !strings.HasPrefix(what, "last: failed to parse Location header") {
			t.Fatalf("answers(a Location that does not parse) = %v, %q; want not ready, for that", ok, what)
		}
		if hide && (what != "last: failed to parse Location header (what the server sent is not said)" || strings.Contains(what, hiddenSecret)) {
			t.Errorf("answers(a Location that echoes the path, hidden) = %q; want what the server sent not said", what)
		}
		if !hide && !strings.Contains(what, hiddenSecret) {
			t.Errorf("answers(a Location that echoes the path, not hidden) = %q; want net/http's reason whole, as before", what)
		}
	})
	bad := srv.URL + "/" + hiddenSecret + "%zz"
	if ok, what := answers(ctx, bad, true, true); ok || what != "the health URL does not parse as a URL" {
		t.Errorf("answers(%q, hidden) = %v, %q; want not ready, quoting no part of it", bad, ok, what)
	}
	if ok, what := answers(ctx, bad, true, false); ok || !strings.Contains(what, hiddenSecret) {
		t.Errorf("answers(%q, not hidden) = %v, %q; want the parser's reason, as before", bad, ok, what)
	}
}

// The readiness gate (the proven switch off) names a health URL by its
// scheme and host only with the path hidden, and as before when not, in
// every line: polling it, its 2xx, its timeout's fallback to the delay or to
// the application's accept, the wait with probing the address disabled, and
// an invalid URL's refusal. A configured health path, which the gate
// requests itself, is said as redacted in its round-trip's lines.
func TestTheReadinessGateNamesAHiddenHealthURLByItsHostOnly(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) {}
	failing := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	// gate runs the gate on a health URL handler serves, polled for at most
	// poll, for an application keploy runs (appRuns: its exit ends the
	// waits) or not. A handler that answers 2xx at once is polled for
	// prTimeout, so that no wall time decides the row; one that never does,
	// for a short poll that ends every time.
	gate := func(t *testing.T, handler http.HandlerFunc, appRuns, hide bool, poll time.Duration, edit func(*config.Config)) (*observer.ObservedLogs, string) {
		t.Helper()
		ctx := context.Background()
		if appRuns {
			ctx = pkg.EndsWithApp(ctx)
		}
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		core, logs := observer.New(zapcore.DebugLevel)
		cfg := hiddenCfg(srv.URL, poll, hide)
		if edit != nil {
			edit(cfg)
		}
		ctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		done := make(chan bool, 1)
		go func() { done <- waitForAppReady(ctx, zap.New(core), cfg, httpProbeTarget{}) }()
		select {
		case ready := <-done:
			if !ready {
				t.Fatal("the gate said stop, want go on")
			}
		case <-time.After(prTimeout):
			t.Fatal("the gate did not decide")
		}
		return logs, namedAs(srv.URL, hide)
	}
	t.Run("its 2xx", func(t *testing.T) {
		eachWay(t, func(t *testing.T, hide bool) {
			logs, want := gate(t, ok, true, hide, prTimeout, nil)
			for _, line := range []string{"polling application health endpoint", "health endpoint reported 2xx"} {
				if got := saidWith(t, logs, line); got != want {
					t.Errorf("%q names %q, want %q", line, got, want)
				}
			}
			keptOut(t, logs, hide)
		})
	})
	t.Run("its timeout falls back to the delay", func(t *testing.T) {
		eachWay(t, func(t *testing.T, hide bool) {
			logs, want := gate(t, failing, false, hide, 200*time.Millisecond, nil)
			if got := saidWith(t, logs, "health probe timed out"); got != want {
				t.Errorf("the timeout line names %q, want %q", got, want)
			}
			keptOut(t, logs, hide)
		})
	})
	t.Run("its timeout waits for the application's accept", func(t *testing.T) {
		eachWay(t, func(t *testing.T, hide bool) {
			logs, want := gate(t, failing, true, hide, 200*time.Millisecond, nil)
			if got := saidWith(t, logs, "has not answered 2xx within the poll timeout; the first test waits instead"); got != want {
				t.Errorf("the fallback line names %q, want %q", got, want)
			}
			keptOut(t, logs, hide)
		})
	})
	t.Run("probing the address disabled, it still waits", func(t *testing.T) {
		eachWay(t, func(t *testing.T, hide bool) {
			calls, _ := recordStillWaiting(t)
			var said atomic.Bool
			var what string
			go func() {
				c := <-calls
				what = c.what
				said.Store(true)
			}()
			logs, want := gate(t, func(w http.ResponseWriter, _ *http.Request) {
				if !said.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}, true, hide, 200*time.Millisecond, func(cfg *config.Config) { cfg.Test.DisableAppReadyProbe = true })
			if got := saidWith(t, logs, "probing the app's address is disabled"); got != want {
				t.Errorf("the disabled-probe line names %q, want %q", got, want)
			}
			if !said.Load() {
				t.Fatal("it never said it still waits")
			}
			if what != "answer 2xx at "+want {
				t.Errorf("it says it still waits to %q, want %q", what, "answer 2xx at "+want)
			}
			keptOut(t, logs, hide)
		})
	})
	t.Run("an invalid one is refused", func(t *testing.T) {
		eachWay(t, func(t *testing.T, hide bool) {
			for _, bad := range invalidHiddenURLs {
				core, logs := observer.New(zapcore.DebugLevel)
				cfg := hiddenCfg("http://localhost:8080", time.Second, hide)
				cfg.Test.HealthURL = bad.url
				if !waitForAppReady(context.Background(), zap.New(core), cfg, httpProbeTarget{}) {
					t.Fatal("the gate said stop, want the delay")
				}
				refusedAs(t, logs, bad, hide)
			}
		})
	})
	t.Run("its health path", func(t *testing.T) {
		// gatePath runs the gate's round-trip stage on host:port, with a
		// health path that holds hiddenSecret, and checks the path the line
		// that holds snippet says it requested.
		gatePath := func(t *testing.T, host, port string, hide bool, ceiling time.Duration, snippet string) {
			t.Helper()
			core, logs := observer.New(zapcore.DebugLevel)
			cfg := gateCfg(ceiling)
			cfg.Test.HealthPath = "/" + hiddenSecret
			cfg.Test.HideHealthURLPath = hide
			if !gateOnAppAddress(pkg.EndsWithApp(context.Background()), zap.New(core), cfg, host, port, "test",
				httpProbeTarget{scheme: "http", host: host, port: port, ok: true}) {
				t.Fatal("the gate said stop, want go on")
			}
			want := "/" + hiddenSecret
			if hide {
				want = "/<path-redacted>"
				saidNowhere(t, logs, hiddenSecret)
			}
			if got := saidField(t, logs, snippet, "probePath"); got != want {
				t.Errorf("%q says it requested %q, want %q", snippet, got, want)
			}
		}
		eachWay(t, func(t *testing.T, hide bool) {
			t.Run("answered", func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(ok))
				t.Cleanup(srv.Close)
				host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
				gatePath(t, host, port, hide, prTimeout, "readiness confirmed by a completed HTTP round-trip")
			})
			t.Run("never answered", func(t *testing.T) {
				host, port := acceptOnlyListener(t)
				gatePath(t, host, port, hide, 300*time.Millisecond, "never completed an HTTP round-trip")
			})
		})
	})
}

// invalidHiddenURLs are invalid health URLs whose path holds hiddenSecret,
// with how a refusal names each one and why, with its path hidden and not:
// one that does not parse, and one that parses with no host.
var invalidHiddenURLs = []struct{ url, named, reason, offNamed, offReason string }{
	{
		"http://localhost:8080/" + hiddenSecret + "%zz", "<unparseable-health-url-redacted>", "it does not parse as a URL",
		"<unparseable-health-url-redacted>", `parse "http://localhost:8080/` + hiddenSecret + `%zz": invalid URL escape "%zz"`,
	},
	{
		"localhost:8080/" + hiddenSecret, "<health-url-redacted>", `unsupported scheme "localhost" (expected http or https)`,
		"localhost:8080/" + hiddenSecret, `unsupported scheme "localhost" (expected http or https)`,
	},
}

// refusedAs fails the row unless logs refuse bad's URL once, naming it and
// saying why as hide has it, and, with its path hidden, say no part of the
// path anywhere.
func refusedAs(t *testing.T, logs *observer.ObservedLogs, bad struct{ url, named, reason, offNamed, offReason string }, hide bool) {
	t.Helper()
	named, reason := bad.named, bad.reason
	if !hide {
		named, reason = bad.offNamed, bad.offReason
	}
	refused := logs.FilterMessageSnippet("invalid --health-url").All()
	if len(refused) != 1 {
		t.Fatalf("%q: %d refusals, want 1: %v", bad.url, len(refused), logs.All())
	}
	if f := refused[0].ContextMap(); f["healthUrl"] != named || f["reason"] != reason {
		t.Errorf("%q: the refusal says %v, want it named %q for %q", bad.url, f, named, reason)
	}
	if hide {
		saidNowhere(t, logs, hiddenSecret)
		saidNowhere(t, logs, "%zz")
	}
}

// Under the proven switch a health URL is named by its scheme and host only
// with the path hidden too, and as before when not: when the agent cannot
// watch and it alone decides, in the line that says the application is
// ready, in the line that says the wait goes on, in the reason of a wait it
// never answered, and in an invalid URL's refusal; and so is the URL a
// configured health path builds.
func TestUnderTheSwitchAHiddenHealthURLIsNamedByItsHostOnly(t *testing.T) {
	answering := func(t *testing.T, after int32) (*httptest.Server, *atomic.Int32) {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if n.Add(1) <= after {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}))
		t.Cleanup(srv.Close)
		return srv, &n
	}
	// held is a server whose every answer (200) waits until release is
	// called: a probe sent at the wait's start cannot end the wait before
	// the row has had the agent's read weighed.
	held := func(t *testing.T) (*httptest.Server, func()) {
		gate := make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(gate) }) }
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-gate:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(release) // runs first: srv.Close never waits on a held answer
		return srv, release
	}
	untilEnd := func(t *testing.T, wake chan struct{}, res chan waitResult) waitResult {
		t.Helper()
		for {
			select {
			case r := <-res:
				return r
			case wake <- struct{}{}:
				time.Sleep(10 * time.Millisecond) // the server answers in wall time
			case <-time.After(30 * time.Second):
				t.Fatal("the wait did not end")
			}
		}
	}
	unsupported := func() *fakeAgent {
		return &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{fmt.Errorf("%w: 501", models.ErrReadinessUnsupported)}}
	}
	// aloneDecides checks the lines of a wait the health URL alone decided,
	// whose URL is named want.
	aloneDecides := func(t *testing.T, logs *observer.ObservedLogs, want string, hide bool) {
		t.Helper()
		if got := saidWith(t, logs, "the health URL alone decides"); got != want {
			t.Errorf("the agent line names %q, want %q", got, want)
		}
		ready := logs.FilterMessageSnippet("the application is ready").All()
		if len(ready) != 1 || ready[0].ContextMap()["ready"] != want+" answered 200" {
			t.Errorf("the ready line says %v, want %q", ready, want+" answered 200")
		}
		keptOut(t, logs, hide)
	}
	t.Run("the agent cannot watch", func(t *testing.T) {
		eachWay(t, func(t *testing.T, hide bool) {
			wake := manualWake(t)
			stallLimit(t, 1000)
			srv, _ := answering(t, 2)
			core, logs := observer.New(zapcore.DebugLevel)
			cfg := hiddenCfg(srv.URL, time.Second, hide)
			res := startWait(provenReadiness{logger: zap.New(core), cfg: cfg, agent: unsupported(), host: "localhost", port: "8080", listen: 8080})
			// Its 2xx needs two wake-ups, sent only once the agent's
			// read is weighed: the agent line is said before the wait
			// can end, whatever the goroutines' order.
			readsWeighed(t, res, 0)
			if r := untilEnd(t, wake, res); !r.ready {
				t.Fatalf("wait = %+v, want ready on the 2xx", r)
			}
			aloneDecides(t, logs, namedAs(srv.URL, hide), hide)
		})
	})
	t.Run("its health path", func(t *testing.T) {
		eachWay(t, func(t *testing.T, hide bool) {
			wake := manualWake(t)
			stallLimit(t, 1000)
			srv, release := held(t)
			host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
			listen, _ := strconv.Atoi(port)
			core, logs := observer.New(zapcore.DebugLevel)
			cfg := hiddenCfg(srv.URL, time.Second, hide)
			cfg.Test.HealthURL = ""
			cfg.Test.HealthPath = "/" + hiddenSecret + "/ready"
			res := startWait(provenReadiness{logger: zap.New(core), cfg: cfg, agent: unsupported(), host: host, port: port, listen: uint16(listen)})
			// With a health path any answer is ready, so the probe sent
			// at the start is held until the agent's read is weighed:
			// else the probe could end the wait before the agent line.
			readsWeighed(t, res, 0)
			release()
			if r := untilEnd(t, wake, res); !r.ready {
				t.Fatalf("wait = %+v, want ready on its answer", r)
			}
			aloneDecides(t, logs, namedAs(srv.URL, hide), hide)
		})
	})
	t.Run("the wait goes on", func(t *testing.T) {
		eachWay(t, func(t *testing.T, hide bool) {
			wake := manualWake(t)
			stallLimit(t, 1000)
			was := readyStatusEvery
			readyStatusEvery = 2
			t.Cleanup(func() { readyStatusEvery = was })
			srv, _ := answering(t, 6)
			core, logs := observer.New(zapcore.DebugLevel)
			a := &fakeAgent{answers: []models.AppReadiness{{Listens: []models.AppListen{{Port: 8080, Here: true}}}}, moving: true}
			if r := untilEnd(t, wake, startWait(provenReadiness{logger: zap.New(core), cfg: hiddenCfg(srv.URL, time.Second, hide), agent: a, host: "localhost", port: "8080", listen: 8080})); !r.ready {
				t.Fatalf("wait = %+v, want ready on the 2xx", r)
			}
			said := logs.FilterMessageSnippet("still waiting for the application").All()
			if len(said) == 0 {
				t.Fatalf("it never said it still waits: %v", logs.All())
			}
			if want := namedAs(srv.URL, hide) + "'s answer"; said[0].ContextMap()["waitingFor"] != want {
				t.Errorf("it says it waits for %v, want %q", said[0].ContextMap()["waitingFor"], want)
			}
			keptOut(t, logs, hide)
		})
	})
	t.Run("it never answered", func(t *testing.T) {
		eachWay(t, func(t *testing.T, hide bool) {
			wake := manualWake(t)
			stallLimit(t, 2)
			srv, n := answering(t, 1<<30)
			core, logs := observer.New(zapcore.DebugLevel)
			res := startWait(provenReadiness{logger: zap.New(core), cfg: hiddenCfg(srv.URL, time.Second, hide), agent: &fakeAgent{answers: []models.AppReadiness{{}}}, host: "localhost", port: "8080", listen: 8080})
			r := wakeUntil(t, wake, res, 100)
			if want := "the application never answered " + namedAs(srv.URL, hide); r.ready || !strings.Contains(r.why, want) {
				t.Fatalf("wait = %+v after %d health requests, want not judged, naming %q", r, n.Load(), want)
			}
			if hide && strings.Contains(r.why, hiddenSecret) {
				t.Errorf("the reason %q holds the path", r.why)
			}
			if !hide && (strings.Contains(r.why, "tok=") || strings.Contains(r.why, "#"+hiddenSecret)) {
				t.Errorf("the reason %q holds the query or fragment", r.why)
			}
			keptOut(t, logs, hide)
		})
	})
	t.Run("an invalid one is refused", func(t *testing.T) {
		t.Setenv("KEPLOY_PROVEN", "1")
		eachWay(t, func(t *testing.T, hide bool) {
			for _, bad := range invalidHiddenURLs {
				core, logs := observer.New(zapcore.DebugLevel)
				cfg := hiddenCfg("http://localhost:8080", time.Second, hide)
				cfg.Test.HealthURL = bad.url
				r := &Replayer{logger: zap.New(core), config: cfg}
				if ready, _ := r.appReady(context.Background(), nil, "test-set-0", false, false); !ready {
					t.Fatal("not ready, want the first test's own connect to decide")
				}
				refusedAs(t, logs, bad, hide)
			}
		})
	})
}
