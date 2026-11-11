package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A credential's last-use touch as the run that lost tests to it recorded
// it: the touch reads the credential by id (mock-216) and by name
// (mock-218, version 1), then writes it with options.cas the version it read
// (mock-221), all on one connection, in TestAppCRUD's window; a declared
// rule placed the three in the worker scope as one once series, mock-216
// first. Every later read by name answers version 2, as the touch left it:
// mock-308 is TestListApps' own. In the replay TestListApps authenticates
// first, so its process makes the touch.
const (
	chainByID   = "/v1/secret/data/keploy/personal-access-tokens/_id/e2e-go-key"
	chainRead   = "/v1/secret/data/keploy/personal-access-tokens/keploy.io/e2e-go-key"
	chainApps   = "/v1/apps"
	chainSeries = "pat-last-use-touch/-/1"

	answerByID    = `{"id":"e2e-go-key"}`
	answerV1      = `{"version":1}`
	answerV2      = `{"version":2}`
	answerTouched = `{"version":2,"touched":true}`
	answerApps    = `[]`
)

// patChain is the touch's staged manager with TestListApps open, its own
// recordings mock-308 (the read by name, version 2) and mock-310 (GET
// /v1/apps). once says whether the scope table marks the series once; edit,
// when not nil, changes the recordings before they are staged.
func patChain(t *testing.T, lg *zap.Logger, once bool, edit func(byName map[string]*models.Mock)) *MockManager {
	t.Helper()
	write := workerProbe("mock-221", 3*time.Second, chainRead, answerTouched)
	write.Spec.HTTPReq.Method = models.Method("PUT")
	write.Spec.HTTPReq.Header["Content-Type"] = "application/json"
	write.Spec.HTTPReq.Body = `{"options":{"cas":1}}`
	mks := []*models.Mock{
		workerProbe("mock-216", time.Second, chainByID, answerByID),
		workerProbe("mock-218", 2*time.Second, chainRead, answerV1),
		write,
		workerProbe("mock-308", 10*time.Second, chainRead, answerV2),
		workerProbe("mock-310", 11*time.Second, chainApps, answerApps),
	}
	if edit != nil {
		byName := map[string]*models.Mock{}
		for _, mk := range mks {
			byName[mk.Name] = mk
		}
		edit(byName)
	}
	mm := stagedManagerLogged(t, lg, nil, mks)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "TestListApps", Start: exactT0.Add(9 * time.Second), End: exactT0.Add(12 * time.Second)}})
	members := []string{"mock-216", "mock-218", "mock-221"}
	mm.ServeExactWorker(members)
	if once {
		mm.SetPoolOnce([]string{chainSeries})
	} else {
		mm.SetPoolOnce(nil)
	}
	mm.SetPoolSeries(map[string][]string{chainSeries: members})
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("TestListApps", []*models.Mock{roundMock(mm, "mock-308"), roundMock(mm, "mock-310")})
	return mm
}

// appConn is one connection of the application's to the HTTP parser, which
// answers its calls in turn from mm.
type appConn struct {
	t     *testing.T
	app   net.Conn
	br    *bufio.Reader
	done  chan struct{}
	ended bool
	once  sync.Once
}

// dialParser opens an appConn; the test's end closes it and waits for the
// parser to return.
func dialParser(t *testing.T, mm *MockManager, lg *zap.Logger) *appConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callDeadline)
	app, agentEnd := net.Pipe()
	c := &appConn{t: t, app: app, br: bufio.NewReader(app), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		_ = httpint.New(lg).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		_ = agentEnd.Close()
	}()
	t.Cleanup(func() {
		c.close()
		cancel()
	})
	return c
}

func (c *appConn) close() {
	c.once.Do(func() {
		_ = c.app.Close()
		<-c.done
	})
}

// send makes the call raw on c and returns the body it was answered with;
// "a miss" for an answer other than 200 and "no answer" when the parser
// ended the connection unanswered. Either ends c, as the parser does: the
// calls after it on c are not made.
func (c *appConn) send(raw string) string {
	c.t.Helper()
	if c.ended {
		return "not made"
	}
	if _, err := io.WriteString(c.app, raw); err != nil {
		c.ended = true
		return "no answer"
	}
	resp, err := http.ReadResponse(c.br, nil)
	switch {
	case err != nil:
		c.ended = true
		return "no answer"
	case resp.StatusCode != http.StatusOK:
		c.ended = true
		return "a miss"
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, resp.ContentLength))
	if err != nil {
		c.t.Fatal(err)
	}
	return string(body)
}

func chainGet(path string) string { return "GET " + path + " HTTP/1.1\r\nHost: deps\r\n\r\n" }

// chainPut is the touch's write, options.cas the version the read answered.
func chainPut(cas int) string {
	b := `{"options":{"cas":` + strconv.Itoa(cas) + `}}`
	return "PUT " + chainRead + " HTTP/1.1\r\nHost: deps\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(b)) + "\r\n\r\n" + b
}

// chainLines are the lines that say a once chain answered a call: each
// one's recording, and whether the open test recorded the call too.
func chainLines(logs *observer.ObservedLogs) []string {
	var out []string
	for _, e := range logs.FilterMessageSnippet("a once series' chain answered a call").All() {
		f := e.ContextMap()
		if f["series"] != chainSeries {
			continue
		}
		out = append(out, f["mock"].(string)+" owned="+strconv.FormatBool(f["the open test recorded this call too"].(bool)))
	}
	return out
}

// Under the proven switch, on the connection where a once series' first
// recording answered a call, the series' next recording answers the next
// call that is that recording, ahead of the open test's own identical
// recording, answered already or not, and so on through the series; a line
// says so for each. So the touch reads version 1 and writes cas 1, as
// recorded. Before, the read was the open test's own (version 2), or, that
// one answered, one call more than the test recorded (a miss), and the
// write missed either way.
func TestAOnceChainsNextAnswersAheadOfTheOpenTestsOwnOnItsConnection(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Run("the test's own read answered first, as in the run", func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		lg := zap.New(core)
		mm := patChain(t, lg, true, nil)
		c := dialParser(t, mm, lg)
		got := []string{c.send(chainGet(chainRead)), c.send(chainGet(chainByID)), c.send(chainGet(chainRead)), c.send(chainPut(1))}
		if want := []string{answerV2, answerByID, answerV1, answerTouched}; !slices.Equal(got, want) {
			t.Fatalf("the connection was answered %v, want %v", got, want)
		}
		if got, want := chainLines(logs), []string{"mock-218 owned=true", "mock-221 owned=false"}; !slices.Equal(got, want) {
			t.Fatalf("the chain's lines are %v, want %v", got, want)
		}
	})
	t.Run("the test's own read still to be answered", func(t *testing.T) {
		mm := patChain(t, zap.NewNop(), true, nil)
		c := dialParser(t, mm, zap.NewNop())
		got := []string{c.send(chainGet(chainByID)), c.send(chainGet(chainRead)), c.send(chainPut(1)), c.send(chainGet(chainRead))}
		if want := []string{answerByID, answerV1, answerTouched, answerV2}; !slices.Equal(got, want) {
			t.Fatalf("the connection was answered %v, want %v", got, want)
		}
	})
}

// No chain in progress on the connection, the open test's own recording
// answers as before: a connection that answered none of the series' first
// recording, one where another call came between, and a series the scope
// table does not mark once. No line says a chain answered.
func TestNoOnceChainInProgressLeavesTheOpenTestsOwnFirst(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name  string
		once  bool
		calls []string
		want  []string
	}{
		{"no chain begun on the connection", true, []string{chainGet(chainRead), chainGet(chainRead)}, []string{answerV2, "a miss"}},
		{"another call between ends the chain", true, []string{chainGet(chainByID), chainGet(chainApps), chainGet(chainRead)}, []string{answerByID, answerApps, answerV2}},
		{"a series not marked once begins none", false, []string{chainGet(chainByID), chainGet(chainRead)}, []string{answerByID, answerV2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			lg := zap.New(core)
			mm := patChain(t, lg, tc.once, nil)
			c := dialParser(t, mm, lg)
			var got []string
			for _, call := range tc.calls {
				got = append(got, c.send(call))
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("the connection was answered %v, want %v", got, tc.want)
			}
			if lines := chainLines(logs); len(lines) != 0 {
				t.Fatalf("a chain line was written: %v", lines)
			}
		})
	}
}

// A chain is its connection's alone: while it is in progress on one
// connection, the same read on another is answered with the open test's
// own recording, and the chain's connection then still reads the chain's.
func TestAnotherConnectionGetsTheOpenTestsOwnWhileAChainIsInProgress(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mm := patChain(t, zap.NewNop(), true, nil)
	a, b := dialParser(t, mm, zap.NewNop()), dialParser(t, mm, zap.NewNop())
	got := []string{a.send(chainGet(chainByID)), b.send(chainGet(chainRead)), a.send(chainGet(chainRead)), a.send(chainPut(1))}
	if want := []string{answerByID, answerV2, answerV1, answerTouched}; !slices.Equal(got, want) {
		t.Fatalf("connections a, b, a, a were answered %v, want %v", got, want)
	}
}

// A chain's next recording that declares variables is the variable pass's,
// which binds them: the chain does not answer it ahead, and the call is
// answered as before, the open test's own first.
func TestAOnceChainsNextThatDeclaresVariablesIsLeftToTheVariablePass(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mm := patChain(t, zap.NewNop(), true, func(byName map[string]*models.Mock) {
		for _, n := range []string{"mock-218", "mock-308"} {
			byName[n].Spec.HTTPReq.Header["X-Req-Id"] = "rec"
		}
		byName["mock-218"].Spec.Variables = models.MockVariables{"rid": {Recorded: "rec", Bind: "header.X-Req-Id"}}
	})
	c := dialParser(t, mm, zap.NewNop())
	got := []string{c.send(chainGet(chainByID)), c.send("GET " + chainRead + " HTTP/1.1\r\nHost: deps\r\nX-Req-Id: rec\r\n\r\n")}
	if want := []string{answerByID, answerV2}; !slices.Equal(got, want) {
		t.Fatalf("the connection was answered %v, want %v", got, want)
	}
}

// A chain's next recording whose answer cannot be loaded ends the call
// unanswered, as any answer that cannot be loaded does: the call is not
// handed to the open test's own recording instead.
func TestAOnceChainsNextWhoseAnswerCannotLoadEndsTheCall(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mm := patChain(t, zap.NewNop(), true, func(byName map[string]*models.Mock) {
		byName["mock-218"].SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
			return nil, nil, errors.New("the spilled answer is gone")
		})
	})
	c := dialParser(t, mm, zap.NewNop())
	got := []string{c.send(chainGet(chainByID)), c.send(chainGet(chainRead))}
	if want := []string{answerByID, "no answer"}; !slices.Equal(got, want) {
		t.Fatalf("the connection was answered %v, want %v", got, want)
	}
}

// OnceLink is where a recording stands in its once series, in placement
// order: the first begins the chain, each names the next, the last none. A
// recording of no series, and every recording of a series not marked once,
// has none.
func TestOnceLinkIsWhereARecordingStandsInItsOnceSeries(t *testing.T) {
	mm := onceTouch(t, true)
	for name, want := range map[string]integrations.OnceLink{
		"p1": {Series: onceID, First: true, Next: "p2"},
		"p2": {Series: onceID, Next: "p3"},
		"p3": {Series: onceID},
	} {
		if got, ok := mm.OnceLink(name); !ok || got != want {
			t.Errorf("OnceLink(%s) = %+v, %v; want %+v, true", name, got, ok, want)
		}
	}
	if got, ok := mm.OnceLink("zz"); ok {
		t.Errorf("OnceLink of a recording of no series = %+v, true", got)
	}
	if got, ok := onceTouch(t, false).OnceLink("p1"); ok {
		t.Errorf("OnceLink of a series not marked once = %+v, true", got)
	}
	bare := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(bare.Close)
	if got, ok := bare.OnceLink("p1"); ok {
		t.Errorf("OnceLink with no series installed = %+v, true", got)
	}
}
