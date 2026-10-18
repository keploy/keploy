package replay

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// aheadInstr is the stand-in agent of a replay under exact serving whose app
// consumes, each time a test case opens, every mock the test case owns.
type aheadInstr struct {
	*prInstr
	owned   map[string][]string
	pending []string
	// failAhead fails the first pool call that names it (ExactServing.Ahead),
	// once recorded; failDrainAt fails the first read of what was consumed
	// after that test case opens.
	failAhead, failDrainAt string
	failDrain              bool
}

func (a *aheadInstr) UpdateMockParams(ctx context.Context, p models.MockFilterParams) error {
	if p.Exact != nil && p.Exact.Test != "" {
		a.mu.Lock()
		a.pending = append([]string(nil), a.owned[p.Exact.Test]...)
		a.failDrain = a.failDrain || p.Exact.Test == a.failDrainAt
		a.mu.Unlock()
	}
	err := a.prInstr.UpdateMockParams(ctx, p)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil && a.failAhead != "" && p.Exact != nil && slices.Contains(p.Exact.Ahead, a.failAhead) {
		a.failAhead = ""
		return errors.New("the agent refused the pool call")
	}
	return err
}

func (a *aheadInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failDrain {
		a.failDrain, a.failDrainAt = false, ""
		return nil, errors.New("the agent could not be read")
	}
	var out []models.MockState
	for _, n := range a.pending {
		out = append(out, models.MockState{Name: n, Kind: models.HTTP, Usage: models.Deleted, IsFiltered: true, Lifetime: models.LifetimePerTest})
	}
	a.pending = nil
	return out, nil
}

// exactSends is what each exact-serving send of p says: "open <test>", or
// "pool" with the calls in flight it names ("pool poll-2").
func exactSends(p []models.MockFilterParams) []string {
	var out []string
	for _, s := range p {
		switch ex := s.Exact; {
		case ex == nil || ex.Stage:
		case ex.Test != "":
			out = append(out, "open "+ex.Test)
		default:
			out = append(out, strings.TrimSpace("pool "+strings.Join(ex.Ahead, ",")))
		}
	}
	return out
}

// aheadOpts shape aheadRun's set. With retry the passing test cases run
// again; unsendable is a test case whose URL has no origin for the base path
// to replace, so it fails unsent; refused is one whose request holds a
// placeholder that belongs in a response, refused when the set loads; with
// resetB the app resets test-2's first request (a docker-proxy reset).
type aheadOpts struct {
	retry               bool
	unsendable, refused string
	resetB              bool
	// failAhead and failDrainAt are aheadInstr's.
	failAhead, failDrainAt string
}

// aheadRun runs, under exact serving, a set of five test cases, test-4 and
// test-5 streaming ones, of which each from test-2 on owns one call in flight when it
// began (poll-N), and returns the exact-serving sends and the harness. test-1
// fails. The stand-in agent flags a test case's calls in flight consumed as it
// opens, as the gap answered them (MockManager.ServeExactTest).
func aheadRun(t *testing.T, o aheadOpts) ([]string, *tmplHarness, *aheadInstr) {
	t.Helper()
	retry, unsendable, refused := o.retry, o.unsendable, o.refused
	var resets atomic.Int32
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		for _, p := range []string{"/a", "/b", "/c"} {
			body := p[1:]
			if p == "/a" {
				body = "not a" // test-1 fails every time: the passing ones run again
			}
			path := p
			mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
				if path == "/b" && o.resetB && resets.Add(1) == 1 {
					// A reset before any answer: SetLinger(0) makes the close
					// an RST, not a FIN.
					if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
						if tcp, ok := c.(*net.TCPConn); ok {
							_ = tcp.SetLinger(0)
						}
						_ = c.Close()
					}
					return
				}
				a.record(r)
				writeJSON(w, http.StatusOK, body)
			})
		}
		for _, p := range []string{"/d", "/e"} {
			mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
				a.record(r)
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte("{\"d\":1}\n"))
			})
		}
	})
	cases := []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/a", nil, "", recorded(200, nil, "a", nil)),
		tmplCase(2, app.addr, "GET", "/b", nil, "", recorded(200, nil, "b", nil)),
		tmplCase(3, app.addr, "GET", "/c", nil, "", recorded(200, nil, "c", nil)),
		// Streaming test cases: deferred to the pass after the cycles.
		tmplCase(4, app.addr, "GET", "/d", nil, "", models.HTTPResp{StatusCode: 200,
			Header: map[string]string{"Content-Type": "application/x-ndjson"}, Body: "{\"d\":1}\n"}),
		tmplCase(5, app.addr, "GET", "/e", nil, "", models.HTTPResp{StatusCode: 200,
			Header: map[string]string{"Content-Type": "application/x-ndjson"}, Body: "{\"d\":1}\n"}),
	}
	conf := &tmplConf{}
	for _, tc := range cases {
		if tc.Name == unsendable {
			tc.HTTPReq.URL = "/b"
		}
		if tc.Name == refused {
			tc.HTTPReq.Body = `{"id":"{{capture string .id}}"}`
			conf.stored = models.TestSet{Template: map[string]interface{}{"id": "u_rec_0001"}}
		}
	}
	// poll-N is sent in the gap before test-N's window and answered inside it.
	poll := func(n int) *models.Mock {
		start := cases[n-1].HTTPReq.Timestamp
		return &models.Mock{Version: "api.keploy.io/v1beta1", Name: "poll-" + string(rune('0'+n)), Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": models.HTTPClient},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://queue/poll"},
			HTTPResp:         &models.HTTPResp{StatusCode: 200},
			ReqTimestampMock: start.Add(-500 * time.Millisecond),
			ResTimestampMock: start.Add(50 * time.Millisecond),
		}, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
	}
	h := newTmplHarness(t, conf, cases, func(c *config.Config) {
		c.RetryPassing = retry
		if unsendable != "" {
			c.Test.BasePath = "http://" + app.addr
		}
	})
	mocks := []*models.Mock{poll(2), poll(3), poll(4), poll(5)}
	h.r.mockDB.(*prMockDB).filtered = mocks
	entry := func(m *models.Mock) models.MockEntry {
		return models.MockEntry{Name: m.Name, Kind: string(m.Kind), ReqTimestampMock: models.FormatMockTimestamp(m.Spec.ReqTimestampMock)}
	}
	h.r.mappingDB.(*prMappingDB).onDisk = map[string][]models.MockEntry{
		"test-1": {}, "test-2": {entry(mocks[0])}, "test-3": {entry(mocks[1])}, "test-4": {entry(mocks[2])}, "test-5": {entry(mocks[3])},
	}
	instr := &aheadInstr{prInstr: h.r.instrumentation.(*prInstr), owned: map[string][]string{"test-2": {"poll-2"}, "test-3": {"poll-3"}, "test-4": {"poll-4"}, "test-5": {"poll-5"}}}
	instr.cases = cases
	instr.failAhead, instr.failDrainAt = o.failAhead, o.failDrainAt
	h.r.instrumentation = instr
	h.r.hookImpl = newHooks(prLogger(), h.r.config, instr, false)
	h.run(t)
	// With a failed read what test-2 consumed is not all known: it has no
	// verdict to check here.
	switch r := h.result(t, "test-2"); {
	case o.failAhead != "" || o.failDrainAt != "":
	case unsendable == "test-2" || refused == "test-2":
		if r.Status != models.TestStatusFailed {
			t.Fatalf("precondition: test-2 is %s; it fails unsent", r.Status)
		}
	case r.Status != models.TestStatusPassed:
		t.Fatalf("test-2 is %s; it passes: %+v", r.Status, r)
	}
	instr.mu.Lock()
	defer instr.mu.Unlock()
	return exactSends(instr.allParams), h, instr
}

// Under exact serving the pool call after each test case names the next one's
// calls in flight when it began (its mapped mocks recorded in the gap before
// its window), and only the next one's. The last test case of a cycle or
// pass, after which the next is not known yet, names none: a retry cycle's
// first test case and the streaming pass's are named at their loop head,
// before their hooks run. So is the one after a test case that was named and
// did not open (it could not be sent). A refused one is never named.
func TestRunTestSet_ThePoolNamesTheNextTestCasesCallsInFlight(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name       string
		retry      bool
		unsendable string
		refused    string
		failAhead  string
		want       []string
	}{
		{name: "a retry cycle and the streaming pass", retry: true, want: []string{
			"open test-1", "pool poll-2", "open test-2", "pool poll-3", "open test-3", "pool",
			// The retry cycle: test-2 opens first, named at its loop head.
			"pool poll-2", "open test-2", "pool poll-3", "open test-3", "pool",
			// The streaming pass: its first test case, named at its loop
			// head; the next named by the pool call after it.
			"pool poll-4", "open test-4", "pool poll-5", "open test-5", "pool",
		}},
		{name: "a test case that could not be sent", unsendable: "test-2", want: []string{
			"open test-1", "pool poll-2",
			// test-2 fails unsent: test-3 is named as it opens.
			"pool poll-3", "open test-3", "pool",
			"pool poll-4", "open test-4", "pool poll-5", "open test-5", "pool",
		}},
		{name: "a refused test case", refused: "test-3", want: []string{
			"open test-1", "pool poll-2", "open test-2",
			// test-3 is refused: it is not next, nor named, and its call in
			// flight stays unanswered.
			"pool",
			"pool poll-4", "open test-4", "pool poll-5", "open test-5", "pool",
		}},
		{name: "a pool call that failed", failAhead: "poll-3", want: []string{
			"open test-1", "pool poll-2", "open test-2",
			// The pool call after test-2 failed: test-3 is named again at
			// its loop head.
			"pool poll-3", "pool poll-3", "open test-3", "pool",
			"pool poll-4", "open test-4", "pool poll-5", "open test-5", "pool",
		}},
		{name: "a refused streaming test case", refused: "test-4", want: []string{
			"open test-1", "pool poll-2", "open test-2", "pool poll-3", "open test-3", "pool",
			// test-4 is refused: not named at its loop head; test-5 is.
			"pool poll-5", "open test-5", "pool",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _, _ := aheadRun(t, aheadOpts{retry: tc.retry, unsendable: tc.unsendable, refused: tc.refused, failAhead: tc.failAhead}); strings.Join(got, " | ") != strings.Join(tc.want, " | ") {
				t.Fatalf("exact sends\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

// A test case whose call in flight the gap answered is flagged consumed as it
// opens; that is read before its request is sent. A reset of the request (a
// docker-proxy reset, before the app saw it) finds nothing consumed since, so
// the request is sent again and the test case judged on its answer, with its
// call in flight among what it consumed.
func TestRunTestSet_AResetRequestIsSentAgainAfterTheGapAnsweredItsCallInFlight(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	_, h, _ := aheadRun(t, aheadOpts{resetB: true})
	r := h.result(t, "test-2")
	if r.Status != models.TestStatusPassed {
		t.Fatalf("test-2 is %s: its reset request was not sent again (%+v)", r.Status, r.Result)
	}
	if len(r.FailureInfo.UnconsumedMocks) != 0 || r.Result.DepsConsumed != 1 {
		t.Fatalf("test-2's call in flight is not among what it consumed: unconsumed %+v, %d consumed", r.FailureInfo.UnconsumedMocks, r.Result.DepsConsumed)
	}
}

// What the gap answered of a test case's calls in flight is read as it opens:
// when that read fails, what the test case consumed is not all known, and it
// has no verdict, as when the read after its request fails.
func TestRunTestSet_ATestCaseWhoseGapAnswersCouldNotBeReadHasNoVerdict(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	_, h, _ := aheadRun(t, aheadOpts{failDrainAt: "test-2"})
	if r := h.result(t, "test-2"); !r.FailureInfo.TestMocksUnread {
		t.Fatalf("test-2 is %s, its consumption read as known (%+v)", r.Status, r.FailureInfo)
	}
}

// A streaming test case reads what the gap answered of its calls in flight as
// it opens, as the main loop's do: they are among what it consumed, and when
// that read fails it has no verdict.
func TestRunTestSet_AStreamingTestCaseCountsWhatTheGapAnsweredOfItsCallsInFlight(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	_, h, _ := aheadRun(t, aheadOpts{})
	for _, name := range []string{"test-4", "test-5"} {
		if r := h.result(t, name); len(r.FailureInfo.UnconsumedMocks) != 0 || r.FailureInfo.TestMocksUnread {
			t.Fatalf("%s is %s: its call in flight is not among what it consumed (%+v)", name, r.Status, r.FailureInfo)
		}
	}
	_, h, _ = aheadRun(t, aheadOpts{failDrainAt: "test-4"})
	if r := h.result(t, "test-4"); !r.FailureInfo.TestMocksUnread {
		t.Fatalf("test-4 is %s, its consumption read as known (%+v)", r.Status, r.FailureInfo)
	}
}
