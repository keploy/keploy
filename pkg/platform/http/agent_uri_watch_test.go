package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// giveUpWithin is how long of wall time a row waits for a watch at an agent
// URI to give up on it. Under fastURILooks the watch gives up at its sixth
// look or so (bootSilence, 5 s, at a second of runnable time a look), the
// looks a millisecond apart: a few milliseconds of looks, or 1.3 s where each
// look waits for an answer that never comes, its wait doubling from 20 ms
// (the longest of these rows). Ten seconds is several times that, with room
// for a loaded box: a wait that never gives up fails its row then, in
// seconds, never only by the package's timeout.
const giveUpWithin = 10 * time.Second

// givenUp is what the watch sent on errs, why it gave up; t fails, saying the
// wait did not give up, once giveUpWithin of wall time has passed with
// nothing sent.
func givenUp(t *testing.T, errs <-chan error) error {
	t.Helper()
	select {
	case err := <-errs:
		return err
	case <-time.After(giveUpWithin):
		t.Fatalf("the wait did not give up within %s of wall time", giveUpWithin)
		return nil
	}
}

// watchURI watches the agent at uri, known only by its URI, which the watch
// must give up on, and returns the lines it logged at INFO and above, with
// why it gave up (givenUp).
func watchURI(t *testing.T, uri string) (*observer.ObservedLogs, error) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return logs, givenUp(t, a.WatchAgentStart(ctx))
}

// acceptOnly listens on a port of its own and hands each connection it takes
// to serve; it closes with the test.
func acceptOnly(t *testing.T, serve func(net.Conn)) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			serve(c)
		}
	}()
	return ln
}

// An agent known only by its URI that takes connections and never answers
// its health check healthy (an answer of another status, a forward in front
// of a port nothing listens on) is given up on once the looks have found no
// new state for bootSilence of keploy's runnable time, saying what it found,
// at WARN and in its error: never a wait that ends only with its caller's
// context.
func TestAnAgentURIThatNeverAnswersHealthyIsGivenUpOn(t *testing.T) {
	cases := []struct {
		name string
		uri  func(t *testing.T) string
		want []string
	}{
		{
			name: "it answers, never healthy",
			uri: func(t *testing.T) string {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "upstream connect error", http.StatusServiceUnavailable)
				}))
				t.Cleanup(srv.Close)
				return srv.URL + "/agent"
			},
			want: []string{"never answered its health check healthy", "answers 503, not healthy", "upstream connect error"},
		},
		{
			// Healthy is 200 alone, as keploy's own health check, the
			// one whose answer ends the wait, takes it
			// (pkg.AgentHealthTicker): an agent that answers 204 never
			// ends the wait, so it is given up on.
			name: "it answers 204",
			uri: func(t *testing.T) string {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}))
				t.Cleanup(srv.Close)
				return srv.URL + "/agent"
			},
			want: []string{"never answered its health check healthy", "answers 204, not healthy"},
		},
		{
			name: "it holds the connection, with no answer",
			uri: func(t *testing.T) string {
				// A frozen agent: the kernel takes the connection, and
				// nothing reads it. Each look waits 20 ms to begin with.
				first := agentLookFirstWait
				agentLookFirstWait = 20 * time.Millisecond
				t.Cleanup(func() { agentLookFirstWait = first })
				var held []net.Conn
				var mu sync.Mutex
				t.Cleanup(func() {
					mu.Lock()
					defer mu.Unlock()
					for _, c := range held {
						_ = c.Close()
					}
				})
				ln := acceptOnly(t, func(c net.Conn) {
					mu.Lock()
					defer mu.Unlock()
					held = append(held, c)
				})
				return "http://" + ln.Addr().String() + "/agent"
			},
			want: []string{"never answered its health check healthy", "holds them with no answer"},
		},
		{
			name: "a forward in front of nothing",
			uri: func(t *testing.T) string {
				ln := acceptOnly(t, func(c net.Conn) { _ = c.Close() })
				return "http://" + ln.Addr().String() + "/agent"
			},
			want: []string{"never answered its health check healthy", "closes them with no answer"},
		},
	}
	// Each on http, and on a scheme an embedder registered on the default
	// transport (k8s-proxy's keploy-agent://), whose looks go through the
	// round tripper it registered: there too what the agent's server does is
	// what the watch says, never that nothing answers.
	for _, tc := range cases {
		for _, registered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, registered scheme %v", tc.name, registered), func(t *testing.T) {
				fastURILooks(t)
				uri := tc.uri(t)
				if registered {
					uri = registerScheme(t, "pod-token") + strings.TrimPrefix(uri, "http")
				}
				watchNeverHealthy(t, uri, tc.want)
			})
		}
	}
}

// watchNeverHealthy watches the agent at uri, which never answers healthy,
// and fails t unless the watch gives up on it saying want, what it found
// once, and that something answers there, in its error and in its WARN.
func watchNeverHealthy(t *testing.T, uri string, want []string) {
	t.Helper()
	logs, err := watchURI(t, uri)
	if err == nil {
		t.Fatal("the watch of an agent that never answers healthy ended saying nothing")
	}
	if !errors.Is(err, pkg.ErrAgentNotReady) {
		t.Fatalf("the watch said %v; want pkg.ErrAgentNotReady", err)
	}
	for _, w := range append([]string{uri}, want...) {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("the watch said %q; want it to say %q", err, w)
		}
	}
	// What it found, said once: the look that found it got no
	// other answer before.
	if strings.Count(err.Error(), want[1]) != 1 || strings.Contains(err.Error(), "its last answer") || strings.Contains(err.Error(), "nothing answers") {
		t.Fatalf("the watch said %q; want what it found said once", err)
	}
	waiting := logs.FilterMessageSnippet("still waiting for the keploy agent at " + uri).All()
	if len(waiting) == 0 || waiting[0].ContextMap()["something_answers"] != true {
		t.Fatalf("said it still waits with %v; want that something answers there", waiting)
	}
	saidOnceAtWarn(t, logs, uri, err)
}

// saidOnceAtWarn fails t unless the one line at WARN or above is the watch's,
// saying the URI as shown and err, why it gave up: a caller may say that error
// only at DEBUG and go on (the runner's release of the agent), so this line is
// the one every caller gets.
func saidOnceAtWarn(t *testing.T, logs *observer.ObservedLogs, shown string, err error) {
	t.Helper()
	said := logs.Filter(func(e observer.LoggedEntry) bool { return e.Level >= zapcore.WarnLevel }).All()
	if len(said) != 1 || said[0].Level != zapcore.WarnLevel || said[0].Message != "giving up on the keploy agent at "+shown || said[0].ContextMap()["error"] != err.Error() {
		t.Fatalf("logged %v at WARN or above; want one WARN, for %s, saying %q", said, shown, err)
	}
}

// An agent that took connections and then takes none (it died, and nothing
// forwards to where it was) is given up on as one that stopped, saying what
// was there before: never as a URI where no agent ever was.
func TestAnAgentURIThatStopsIsGivenUpOnAsStopped(t *testing.T) {
	fastURILooks(t)
	// keploy's runnable time stands still while it lives (a box that has
	// not run keploy since), so only its stop, and the time that runs
	// after it, can end the wait.
	clk := &stall.ManualClock{}
	uriClock = clk
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	uri := srv.URL + "/agent"
	core, logs := observer.New(zapcore.InfoLevel)
	a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := a.WatchAgentStart(ctx)
	// Looks find it answering until the watch says it still waits, at its
	// fifth look (fastURILooks: a look a millisecond), then it dies.
	deadline := time.Now().Add(10 * time.Second)
	for logs.FilterMessageSnippet("still waiting").Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	srv.Close()
	deadline = time.Now().Add(giveUpWithin)
	for {
		select {
		case err := <-errs:
			if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "the keploy agent at "+uri+" has stopped") || strings.Count(err.Error(), "its health check answers 503, not healthy: \"starting\"") != 1 {
				t.Fatalf("the watch said %v; want that the agent at the URI has stopped, and what it answered before, once", err)
			}
			saidOnceAtWarn(t, logs, uri, err)
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the wait did not give up within %s of wall time on an agent that stopped", giveUpWithin)
		}
		clk.Advance(100 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
}

// A token an embedder sealed into the agent's URI (k8s-proxy's
// keploy-agent://, its password) is never said, nor any part of it: the
// watch's lines, at any level, and its reason show it as xxxxx, whatever its
// shape. Where a raw character in the password leaves the URI unparsed, or
// parsed with the password as something else (a path, or no host at all), the
// errors of url.Parse and of the request that keploy says quote it, so each
// goes through the mask.
func TestAnAgentURIsSealedTokenIsNeverSaid(t *testing.T) {
	cases := map[string]struct{ uri, shown string }{
		"a URI that parses":                {"http://:s3aled-t0ken@%s/agent", "http://:xxxxx@%s/agent"},
		"a URI that does not parse":        {"keploy-agent://:s3aled%%zzt0ken@%s/agent", "keploy-agent://xxxxx@%s/agent"},
		"no host part":                     {"agent:s3aled-t0ken@%s/agent", "xxxxx@%s/agent"},
		"a raw / in the password":          {"keploy-agent://:s3aled/t0ken@%s/agent", "keploy-agent://xxxxx@%s/agent"},
		"a raw ? in the password":          {"keploy-agent://:s3aled?t0ken@%s/agent", "keploy-agent://xxxxx@%s/agent"},
		"a raw # in the password":          {"http://u:s3aled#t0ken@%s/agent", "http://xxxxx@%s/agent"},
		"digits, then a / in the password": {"http://:1/s3aled-t0ken@%s/agent", "http://xxxxx@%s/agent"},
		"digits, then a # in the password": {"http://:1#s3aled-t0ken@%s/agent", "http://xxxxx@%s/agent"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fastURILooks(t)
			addr := testnet.RefusedAddr(t, "tcp")
			core, logs := observer.New(zapcore.DebugLevel)
			a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: fmt.Sprintf(tc.uri, addr)}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := givenUp(t, a.WatchAgentStart(ctx))
			if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "nothing answers at "+fmt.Sprintf(tc.shown, addr)+":") {
				t.Fatalf("the watch said %v; want that nothing answers at the URI, its password shown as xxxxx", err)
			}
			if logs.FilterMessageSnippet("still waiting for the keploy agent at "+fmt.Sprintf(tc.shown, addr)).Len() == 0 {
				t.Fatalf("logged %v; want the still-waiting line, its URI masked", logs.All())
			}
			saidOnceAtWarn(t, logs, fmt.Sprintf(tc.shown, addr), err)
			said := []string{err.Error()}
			for _, l := range logs.All() {
				said = append(said, l.Message, fmt.Sprint(l.ContextMap()))
			}
			for _, s := range said {
				for _, part := range []string{"s3aled", "t0ken", "%zz"} {
					if strings.Contains(s, part) {
						t.Fatalf("said %q, with a part of the URI's password (%s)", s, part)
					}
				}
			}
		})
	}
}

// socketLooks makes each look at the URI a real one (lookAtAgentHealth), on
// the real socket at(i) names for look i (from 0), and returns the count of
// looks.
func socketLooks(t *testing.T, at func(i int) string) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := lookAtAgent
	lookAtAgent = func(ctx context.Context, c *http.Client, _ string, w time.Duration) agentLook {
		return lookAtAgentHealth(ctx, c, at(int(n.Add(1))-1)+"/health", w)
	}
	t.Cleanup(func() { lookAtAgent = prev })
	return &n
}

// answers503 is the URL of a server that answers every request 503, and what
// stops it.
func answers503(t *testing.T) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, srv.Close
}

// closesEach is the URL of a port that closes each connection it takes, with
// no answer, and what stops it.
func closesEach(t *testing.T) (string, func()) {
	t.Helper()
	ln := acceptOnly(t, func(c net.Conn) { _ = c.Close() })
	return "http://" + ln.Addr().String(), func() { _ = ln.Close() }
}

// The bound is that the looks found no new state for bootSilence. A refused
// look is no new state once one was refused, so the look that ends the wait
// can come soon after the last connection taken: the reason says that time,
// measured from that look, on real sockets, and never takes the bound for it.
// Each look is a second of runnable time, the bound 5 s, so the wait ends 5 s
// past the last new state.
func TestAnAgentURIsReasonSaysHowLongSinceItsLastTakenConnection(t *testing.T) {
	const answered, closed = `its health check answers 503, not healthy: "starting"`, "closes them with no answer"
	cases := []struct {
		name  string
		serve func(*testing.T) (string, func())
		// taken says whether look i (from 0) goes to the server, where
		// the others go to a port that refuses.
		taken func(i int) bool
		// diesAt is the look the server is stopped before, if any.
		diesAt int
		want   []string
	}{
		{
			// A first look at a starting agent usually is refused, so
			// its death is no new state.
			name:   "it refuses, serves 503 three times, and dies",
			serve:  answers503,
			taken:  func(i int) bool { return i >= 1 },
			diesAt: 4,
			want: []string{"has stopped", "none was taken for 3s of keploy's runnable time since", "found nothing new there for 5s",
				"the last look that found something there: " + answered},
		},
		{
			name:  "it refuses and serves 503 in turn",
			serve: answers503,
			taken: func(i int) bool { return i%2 == 1 },
			want: []string{"has stopped", "none was taken for 1s of keploy's runnable time since", "found nothing new there for 5s",
				"the last look that found something there: " + answered},
		},
		{
			name:  "a forward comes and goes",
			serve: closesEach,
			taken: func(i int) bool { return i%2 == 1 },
			want: []string{"has stopped", "none was taken for 1s of keploy's runnable time since", "found nothing new there for 5s",
				"the last look that found something there: something takes connections there and " + closed},
		},
		{
			// The look that ends the wait found an answer: nothing has
			// stopped, whatever looks before it found.
			name:  "it serves 503 and refuses in turn",
			serve: answers503,
			taken: func(i int) bool { return i%2 == 0 },
			want:  []string{"never answered its health check healthy", "found nothing new there for 5s", answered},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastURILooks(t)
			server, stop := tc.serve(t)
			refused := "http://" + testnet.RefusedAddr(t, "tcp")
			n := socketLooks(t, func(i int) string {
				if tc.diesAt != 0 && i == tc.diesAt {
					stop()
				}
				if tc.taken(i) {
					return server
				}
				return refused
			})
			_, err := watchURI(t, "http://127.0.0.1:1/agent")
			if !errors.Is(err, pkg.ErrAgentNotReady) {
				t.Fatalf("the watch said %v; want pkg.ErrAgentNotReady", err)
			}
			if got := n.Load(); got != 7 {
				t.Fatalf("the wait ended at look %d; want the 7th, 5 s past the second's new state: %v", got, err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("the watch said %q; want it to say %q", err, w)
				}
			}
		})
	}
}

// Healthy time is never silence: however long an agent has answered healthy,
// and however many states it showed before, one look that finds a state it
// showed before does not end the wait. Only the bound, from its last healthy
// look, does.
func TestAHealthyLookStartsAnAgentURIsBoundAgain(t *testing.T) {
	fastURILooks(t)
	busy := agentLook{kind: agentAnswered, status: http.StatusServiceUnavailable, found: "scripted 503"}
	healthy := agentLook{kind: agentHealthy, status: http.StatusOK, found: "scripted healthy"}
	var script []agentLook
	script = append(script, busy)
	for range 40 {
		script = append(script, healthy)
	}
	// The bound is 5 looks: 4 of a seen state are under it, each time.
	script = append(script, busy, busy, busy, busy, healthy, busy, busy, busy, busy, healthy)
	n, err := watchScriptedLooks(t, script, busy)
	if got, want := int(n), len(script)+5; got != want {
		t.Fatalf("the wait ended at look %d; want %d, the bound past the last healthy look: %v", got, want, err)
	}
	if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "no longer answers its health check healthy") {
		t.Fatalf("the watch said %v; want that the agent no longer answers healthy", err)
	}
}

// The release of a healthy agent is not given up on for a blip: an agent whose
// /agent/ready takes longer than the bound, and whose health check blips
// twice with a status it gave before, is released.
func TestTheReleaseOfAHealthyAgentThatBlipsIsNotGivenUpOn(t *testing.T) {
	fastURILooks(t)
	var n atomic.Int32
	prev := lookAtAgent
	lookAtAgent = func(context.Context, *http.Client, string, time.Duration) agentLook {
		if i := n.Add(1); i == 1 || i == 11 || i == 21 {
			return agentLook{kind: agentAnswered, status: http.StatusServiceUnavailable, found: "scripted 503"}
		}
		return agentLook{kind: agentHealthy, status: http.StatusOK, found: "scripted healthy"}
	}
	t.Cleanup(func() { lookAtAgent = prev })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent/ready") && n.Load() < 30 {
			http.Error(w, "not yet", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KEPLOY_PROVEN", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*giveUpWithin)
	defer cancel()
	if err := clientFor(srv.URL).MakeAgentReadyForDockerCompose(ctx); err != nil {
		t.Fatalf("the release of a healthy agent ended with %v; want it released", err)
	}
}

// The watch's WARN is the one line every caller gets: the release's callers
// may go on past its error, and the runner says it only at DEBUG. An agent
// that freezes at the release is given up on, and the WARN says why, with its
// URI masked.
func TestAFrozenAgentAtItsReleaseIsSaidAtWarn(t *testing.T) {
	fastURILooks(t)
	prev := lookAtAgent
	lookAtAgent = func(context.Context, *http.Client, string, time.Duration) agentLook {
		return agentLook{kind: agentHeld, found: "scripted: holds them with no answer"}
	}
	t.Cleanup(func() { lookAtAgent = prev })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not yet", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KEPLOY_PROVEN", "")
	host := strings.TrimPrefix(srv.URL, "http://")
	core, logs := observer.New(zapcore.InfoLevel)
	a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: "http://:s3aled-t0ken@" + host}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*giveUpWithin)
	defer cancel()
	err := a.MakeAgentReadyForDockerCompose(ctx)
	if !errors.Is(err, pkg.ErrAgentNotReady) {
		t.Fatalf("the release ended with %v; want pkg.ErrAgentNotReady", err)
	}
	said := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("giving up on the keploy agent at http://:xxxxx@" + host).All()
	if len(said) != 1 || !strings.Contains(fmt.Sprint(said[0].ContextMap()["error"]), "scripted: holds them with no answer") {
		t.Fatalf("logged %v; want one WARN that says why, its URI masked", logs.All())
	}
	if strings.Contains(fmt.Sprint(logs.All()), "s3aled") {
		t.Fatalf("logged %v, with a part of the URI's password", logs.All())
	}
}

// Every request this package makes goes through keploy's mask, by
// construction: their errors go to log lines and reasons from dozens of
// places, and url.Parse's and the request's quote the URI.
func TestEveryAgentRequestGoesThroughTheMask(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("the package's files: %v, %v", files, err)
	}
	raw := regexp.MustCompile(`\bhttp\.NewRequest(WithContext)?\(|\.Do\(req|[cC]lient\.(Get|Head|Post|PostForm)\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "agent_request.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if raw.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				t.Errorf("%s:%d makes or sends a request outside newAgentRequest and doAgent: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// keploy's wait for an agent compose started again says the URI as shown,
// when its context ends and when the watch gives up on it.
func TestTheWaitForARestartedAgentNeverSaysTheURIsPassword(t *testing.T) {
	cases := map[string]struct {
		clk    stall.Clock
		within time.Duration
		want   string
	}{
		// No runnable time passes, so only the context can end the wait.
		"its context ends":   {&stall.ManualClock{}, 300 * time.Millisecond, "did not answer at"},
		"the watch gives up": {&steppingClock{}, giveUpWithin, "did not come up at"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fastURILooks(t)
			uriClock = tc.clk
			addr := testnet.RefusedAddr(t, "tcp")
			ctx, cancel := context.WithTimeout(context.Background(), tc.within)
			defer cancel()
			err := clientFor("http://:s3aled-t0ken@" + addr + "/agent").awaitAgentAnswers(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want+" http://:xxxxx@"+addr+"/agent") || strings.Contains(err.Error(), "s3aled") {
				t.Fatalf("the wait said %v; want %q, its URI masked", err, tc.want)
			}
		})
	}
}

// The pcap stream's lines and errors say its URL as shown: it is made from the
// agent's URI, and streamed for k8s, where that URI has a password.
func TestThePcapStreamNeverSaysTheURIsPassword(t *testing.T) {
	refused := testnet.RefusedAddr(t, "tcp")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/none") {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "broken", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	cases := map[string]struct{ addr, path, want string }{
		"the agent cannot be dialled": {refused, "/pcap", "dial http://:xxxxx@" + refused + "/agent/pcap"},
		"the stream answers an error": {host, "/pcap", "stream http://:xxxxx@" + host + "/agent/pcap: status 500"},
		"the agent has no stream":     {host, "/none", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: "http://:s3aled-t0ken@" + tc.addr + "/agent"}})
			err := a.streamPcapArtifact(context.Background(), tc.path, filepath.Join(t.TempDir(), "out"), 0o600, false)
			switch {
			case tc.want == "" && err != nil,
				tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("the stream said %v; want %q", err, tc.want)
			case tc.want == "" && logs.FilterField(zap.String("url", "http://:xxxxx@"+tc.addr+"/agent"+tc.path)).Len() == 0:
				t.Fatalf("logged %v; want a line with the stream's URL masked", logs.All())
			}
			said := fmt.Sprint(err, logs.All())
			if strings.Contains(said, "s3aled") {
				t.Fatalf("said %s, with a part of the URI's password", said)
			}
		})
	}
}

// A look at an agent known only by its URI that ran out of time with no
// answer (held) makes the next look wait twice as long, up to
// agentLookMaxWait; any other look keeps the wait as it is.
func TestNextAgentLookWait(t *testing.T) {
	cases := []struct {
		wait time.Duration
		held bool
		want time.Duration
	}{
		{time.Second, false, time.Second},
		{time.Second, true, 2 * time.Second},
		{14 * time.Second, true, 28 * time.Second},
		{15 * time.Second, true, agentLookMaxWait},
		{16 * time.Second, true, agentLookMaxWait},
		{agentLookMaxWait, true, agentLookMaxWait},
		{agentLookMaxWait, false, agentLookMaxWait},
	}
	for _, tc := range cases {
		if got := nextAgentLookWait(tc.wait, tc.held); got != tc.want {
			t.Errorf("nextAgentLookWait(%s, held %v) = %s; want %s", tc.wait, tc.held, got, tc.want)
		}
	}
}

// The watch of an agent known only by its URI gives each look the wait the
// last look's left it: from agentLookFirstWait, twice as long after each look
// that held the connection with no answer, up to agentLookMaxWait, and as long
// after any other look (one that found it closed, or an answer), as keploy's
// own health check does, so an agent behind a slow path is found answering.
func TestAnAgentURILookWaitsLongerAfterEachHeldLook(t *testing.T) {
	fastURILooks(t)
	script := []agentLookKind{agentHeld, agentHeld, agentHeld, agentClosed, agentHeld, agentAnswered, agentHeld, agentHeld}
	var (
		mu    sync.Mutex
		waits []time.Duration
		n     atomic.Int32
	)
	prev := lookAtAgent
	lookAtAgent = func(_ context.Context, _ *http.Client, _ string, wait time.Duration) agentLook {
		mu.Lock()
		defer mu.Unlock()
		defer n.Add(1)
		waits = append(waits, wait)
		if i := len(waits) - 1; i < len(script) {
			return agentLook{kind: script[i], found: "scripted"}
		}
		return agentLook{kind: agentHealthy, status: http.StatusOK, found: "scripted"}
	}
	t.Cleanup(func() { lookAtAgent = prev })
	watchScripted(t, &n, len(script)+2)
	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 16 * time.Second, 16 * time.Second, agentLookMaxWait, agentLookMaxWait, agentLookMaxWait}
	if fmt.Sprint(waits[:len(want)]) != fmt.Sprint(want) {
		t.Fatalf("the looks waited %v; want %v", waits[:len(want)], want)
	}
}

// watchScriptedLooks watches an agent known only by its URI whose looks find
// before, then then for good, which the watch must give up on, and returns the
// count of looks, with why it gave up (givenUp).
func watchScriptedLooks(t *testing.T, before []agentLook, then agentLook) (int32, error) {
	t.Helper()
	var n atomic.Int32
	prev := lookAtAgent
	lookAtAgent = func(context.Context, *http.Client, string, time.Duration) agentLook {
		if i := int(n.Add(1)) - 1; i < len(before) {
			return before[i]
		}
		return then
	}
	t.Cleanup(func() { lookAtAgent = prev })
	_, err := watchURI(t, "http://127.0.0.1:1/agent")
	return n.Load(), err
}

// The reason the watch gives up with says what the looks found before the
// last: an agent that answered healthy and then stalls no longer answers
// healthy (it is never said to have never answered so), and its last answer
// is said where the last look that found something take its connection got
// none, and only there.
func TestAnAgentURIsReasonSaysWhatItFoundBefore(t *testing.T) {
	healthy := agentLook{kind: agentHealthy, status: http.StatusOK, found: "its health check answers healthy"}
	starting := agentLook{kind: agentAnswered, status: http.StatusServiceUnavailable, found: "answers 503: starting"}
	held := agentLook{kind: agentHeld, found: "holds it"}
	closed := agentLook{kind: agentClosed, found: "closes it"}
	refused := agentLook{kind: agentRefused, found: "refuses it"}
	cases := []struct {
		name      string
		before    []agentLook
		then      agentLook
		want, not []string
	}{
		{
			name: "healthy, then held", before: []agentLook{starting, healthy}, then: held,
			want: []string{"no longer answers its health check healthy", ": holds it; its last answer: its health check answers healthy"},
			not:  []string{"never answered"},
		},
		{
			name: "answering, then closed", before: []agentLook{starting}, then: closed,
			want: []string{"never answered its health check healthy", ": closes it; its last answer: answers 503: starting"},
			not:  []string{"no longer"},
		},
		{
			name: "healthy, then stopped", before: []agentLook{starting, healthy}, then: refused,
			want: []string{"has stopped", "(refuses it); keploy found nothing new there for 5s; the last look that found something there: its health check answers healthy"},
			not:  []string{"its last answer"},
		},
		{
			name: "answering, then held, then stopped", before: []agentLook{starting, held}, then: refused,
			want: []string{"has stopped", "; the last look that found something there: holds it; its last answer: answers 503: starting"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastURILooks(t)
			_, err := watchScriptedLooks(t, tc.before, tc.then)
			if !errors.Is(err, pkg.ErrAgentNotReady) {
				t.Fatalf("the watch said %v; want pkg.ErrAgentNotReady", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("the watch said %q; want it to say %q", err, w)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(err.Error(), w) {
					t.Fatalf("the watch said %q; want it not to say %q", err, w)
				}
			}
		})
	}
}

// The wait is given up on at the first look bootSilence of keploy's runnable
// time past the first look that found what the looks find since, however long
// keploy ran before the watch began: not at once, and not a look later.
func TestAnAgentURIIsGivenUpOnAtBootSilenceFromTheFirstLook(t *testing.T) {
	fastURILooks(t)
	clk := &steppingClock{}
	clk.Advance(time.Hour)
	uriClock = clk
	looks, err := watchScriptedLooks(t, nil, agentLook{kind: agentHeld, found: "holds it"})
	// A second of runnable time a look: the 6th is 5 s, bootSilence, past
	// the first.
	if !errors.Is(err, pkg.ErrAgentNotReady) || looks != 6 {
		t.Fatalf("the watch said %v after %d looks; want pkg.ErrAgentNotReady after 6", err, looks)
	}
}

// An agent URI its health check cannot be asked at (no scheme keploy can send
// on, or one that does not parse) is given up on as one where nothing
// answers, saying so: never a wait with no watch, which only its caller's
// context would end.
func TestAnAgentURIItsHealthCheckCannotBeAskedAtIsGivenUpOn(t *testing.T) {
	cases := map[string]string{"no scheme": "localhost:16789", "does not parse": "http://[::1:16789"}
	for name, uri := range cases {
		t.Run(name, func(t *testing.T) {
			fastURILooks(t)
			logs, err := watchURI(t, uri)
			if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "nothing answers at "+uri+":") {
				t.Fatalf("the watch said %v; want that nothing answers at %s", err, uri)
			}
			waiting := logs.FilterMessageSnippet("still waiting for the keploy agent at " + uri).All()
			if len(waiting) == 0 || waiting[0].ContextMap()["something_answers"] != false {
				t.Fatalf("said it still waits with %v; want that nothing answers there", waiting)
			}
		})
	}
}

// A look on a connection kept from an earlier look (the round tripper an
// embedder registered for its scheme keeps its connections, as k8s-proxy's
// does) that the agent then holds with no answer finds it held, not refused:
// the connection was taken, though no dial connected it for this look. The
// look waits its own wait for the answer.
func TestALookOnAKeptConnectionTheAgentHoldsFindsItHeld(t *testing.T) {
	var asked, conns atomic.Int32
	hold := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked.Add(1) == 1 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(hold) })
	healthURL := registerScheme(t, "pod-token") + "://" + srv.Listener.Addr().String() + "/agent/health"
	client := &http.Client{Transport: ownConnections(http.DefaultTransport)}
	if l := lookAtAgentHealth(context.Background(), client, healthURL, 5*time.Second); l.kind != agentAnswered {
		t.Fatalf("the first look found %+v; want an answer", l)
	}
	// The look waits its own wait, not the first look's.
	first := agentLookFirstWait
	agentLookFirstWait = time.Hour
	t.Cleanup(func() { agentLookFirstWait = first })
	looked := make(chan agentLook, 1)
	go func() { looked <- lookAtAgentHealth(context.Background(), client, healthURL, 100*time.Millisecond) }()
	var l agentLook
	select {
	case l = <-looked:
	case <-time.After(30 * time.Second):
		t.Fatal("the look waited past its wait")
	}
	if conns.Load() != 1 {
		t.Fatalf("the looks took %d connections; want the second on the first's", conns.Load())
	}
	if l.kind != agentHeld {
		t.Fatalf("the look on the kept connection found %+v; want it held", l)
	}
}

// An answer of a status the wait has not seen is a step of the agent's
// start: one whose health check answers 599, then 598, and so on, each for
// under bootSilence, is waited for, past bootSilence many times over, until
// it answers healthy.
func TestAnAgentURIWhoseAnswerChangesIsWaitedFor(t *testing.T) {
	fastURILooks(t)
	var n atomic.Int32
	prev := lookAtAgent
	lookAtAgent = func(context.Context, *http.Client, string, time.Duration) agentLook {
		i := int(n.Add(1)) - 1
		if i >= 24 {
			return agentLook{kind: agentHealthy, status: http.StatusOK, found: "scripted"}
		}
		// 4 looks at each status, under the 5 of bootSilence.
		return agentLook{kind: agentAnswered, status: 599 - i/4, found: "scripted"}
	}
	t.Cleanup(func() { lookAtAgent = prev })
	watchScripted(t, &n, 24+20)
}

// An agent that never answers healthy and whose answers go back and forth
// (503, then 502, then 503 again...) makes no progress past its second
// status: the wait is given up on bootSilence of keploy's runnable time
// after the first look that found 502, at the 10th look (a second a look),
// as never healthy; never waited for as long as the answers keep turning.
func TestAnAgentURIWhoseAnswersGoBackAndForthIsGivenUpOn(t *testing.T) {
	fastURILooks(t)
	var n atomic.Int32
	prev := lookAtAgent
	lookAtAgent = func(context.Context, *http.Client, string, time.Duration) agentLook {
		i := int(n.Add(1)) - 1
		// 4 looks at each status, under the 5 of bootSilence.
		status := http.StatusServiceUnavailable - (i/4)%2
		return agentLook{kind: agentAnswered, status: status, found: fmt.Sprintf("its health check answers %d, not healthy", status)}
	}
	t.Cleanup(func() { lookAtAgent = prev })
	_, err := watchURI(t, "http://127.0.0.1:1/agent")
	if looks := n.Load(); !errors.Is(err, pkg.ErrAgentNotReady) || looks != 10 || !strings.Contains(err.Error(), "never answered its health check healthy") {
		t.Fatalf("the watch said %v after %d looks; want that it never answered healthy, after 10", err, looks)
	}
}

// A look whose dial is never taken within the look's wait (an address that
// drops what is sent to it: a deleted pod's stale IP, a network policy's
// drop) finds that nothing takes a connection there, as a refused one does:
// never that something holds it, and the next look waits as long as it did.
func TestALookWhoseDialIsNeverTakenFindsNothingThere(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-stop:
			return nil, errors.New("the test is over")
		}
	}}}
	wait := 50 * time.Millisecond
	l := lookAtAgentHealth(context.Background(), client, "http://192.0.2.1:9/agent/health", wait)
	if l.kind != agentRefused || !strings.Contains(l.found, "nothing takes a connection there") {
		t.Fatalf("the look found %+v; want that nothing takes a connection there", l)
	}
	if next := nextAgentLookWait(wait, l.kind == agentHeld); next != wait {
		t.Fatalf("the next look waits %s; want %s, as this one did", next, wait)
	}
}

// Each look at an agent known only by its URI, on http, goes on a connection
// of its own: never one kept from an earlier request on the default
// transport, which keploy's own health check sends on (pkg.AgentHealthTicker),
// so a look cut off at its wait's end never closes a connection another
// request may be on (ownConnections).
func TestEachLookAtAnAgentURIIsOnAConnectionOfItsOwn(t *testing.T) {
	fastURILooks(t)
	var asked, conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	if _, err := watchURI(t, srv.URL+"/agent"); !errors.Is(err, pkg.ErrAgentNotReady) {
		t.Fatalf("the watch said %v; want pkg.ErrAgentNotReady", err)
	}
	if asked.Load() < 2 || conns.Load() < asked.Load() {
		t.Fatalf("%d looks took %d connections; want one each", asked.Load(), conns.Load())
	}
}
