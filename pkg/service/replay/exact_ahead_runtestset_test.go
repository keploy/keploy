package replay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
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
	// learnedOnce: the state of poll-2 the agent gives after test-2's first
	// opening says it learned a field of its request (MockState.ReqBodyNoise);
	// after a later opening, the same mock carries nothing learned, as a
	// retry cycle's learn step learns nothing. opens counts each test's.
	learnedOnce bool
	opens       map[string]int
	// gapLearns: after every pool call that leaves the learn step on, the
	// application makes a call between test cases (a heartbeat, hb-K for the
	// K-th such pool call) the pool answers and the learn step learns a
	// field of. It is made after the read of what was consumed that follows
	// the pool call (the ended test case's), so the read after that gives it.
	gapLearns  bool
	gap, later []string
	poolCalls  int
	// reconsumed is a mock every test case opened with the learn step ended
	// consumes again, its state then carrying nothing learned.
	reconsumed string
	// events is viewInstr's trace of what the replay asks of the agent:
	// each view ("stage", "pool", "test:<name>"; a pool call with the learn
	// step ended is "learn-ended" then "pool"), each test's capture window
	// opened ("begin", "continue": the learn tracker begins it) and each read
	// of what was consumed ("consumed").
	events []string
	// unreadAt is a test case whose opening filter the agent answers
	// without saying its learn step took the request.
	unreadAt string
	// lateLearnAt is a test case whose mocks, consumed after an opening with
	// the learn step ended, a parser learned on all the same: their states
	// say so (MockState.LearnedAfterEnd). late holds those to be read;
	// ended says a filter ended the set's learn step (it stays ended).
	// lateInStream holds them (heldLate) out of every read until the pool
	// is served again (untilPool): a streaming test case's read after its
	// answer's body is read whole, whose end serves the pool.
	lateLearnAt  string
	late         map[string]bool
	ended        bool
	lateInStream bool
	heldLate     []string
	untilPool    bool
}

func (a *aheadInstr) BeginTestErrorCapture(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "begin")
	return nil
}

func (a *aheadInstr) ContinueTestErrorCapture(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "continue")
	return nil
}

func (a *aheadInstr) UpdateMockParams(ctx context.Context, p models.MockFilterParams) error {
	a.mu.Lock()
	a.ended = a.ended || p.LearnEnded
	a.mu.Unlock()
	if p.Exact != nil && p.Exact.Test != "" {
		a.mu.Lock()
		a.pending = append([]string(nil), a.owned[p.Exact.Test]...)
		if p.LearnEnded && a.reconsumed != "" {
			a.pending = append(a.pending, a.reconsumed)
		}
		a.failDrain = a.failDrain || p.Exact.Test == a.failDrainAt
		if a.ended && p.Exact.Test == a.lateLearnAt {
			if a.late == nil {
				a.late = map[string]bool{}
			}
			for _, n := range a.owned[p.Exact.Test] {
				a.late[n] = true
			}
			if a.lateInStream {
				a.heldLate, a.untilPool = a.pending, true
				a.pending = nil
			}
		}
		if a.opens == nil {
			a.opens = map[string]int{}
		}
		a.opens[p.Exact.Test]++
		a.mu.Unlock()
	}
	err := a.prInstr.UpdateMockParams(ctx, p)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil && p.Exact != nil {
		switch {
		case p.Exact.Stage:
			a.events = append(a.events, "stage")
		case p.Exact.Test != "":
			a.events = append(a.events, "test:"+p.Exact.Test)
		default:
			if p.LearnEnded {
				a.events = append(a.events, "learn-ended")
			}
			a.events = append(a.events, "pool")
			a.untilPool = false
		}
	}
	if err == nil && a.gapLearns && p.Exact != nil && !p.Exact.Stage && p.Exact.Test == "" && !p.LearnEnded {
		a.poolCalls++
		a.later = append(a.later, fmt.Sprintf("hb-%d", a.poolCalls))
	}
	if err == nil && a.failAhead != "" && p.Exact != nil && slices.Contains(p.Exact.Ahead, a.failAhead) {
		a.failAhead = ""
		return errors.New("the agent refused the pool call")
	}
	return err
}

// SendTestCaseFilter is UpdateMockParams, whose answer says the agent's learn
// step took the test case's request where learnedOnce is set (an agent of
// this keploy's version), and does not say so otherwise.
func (a *aheadInstr) SendTestCaseFilter(ctx context.Context, p models.MockFilterParams) (bool, error) {
	a.mu.Lock()
	read := a.learnedOnce && (p.Exact == nil || p.Exact.Test != a.unreadAt)
	a.mu.Unlock()
	return read, a.UpdateMockParams(ctx, p)
}

func (a *aheadInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "consumed")
	if a.failDrain {
		a.failDrain, a.failDrainAt = false, ""
		return nil, errors.New("the agent could not be read")
	}
	var out []models.MockState
	if len(a.heldLate) > 0 && !a.untilPool {
		a.pending = append(a.pending, a.heldLate...)
		a.heldLate = nil
	}
	for _, n := range a.pending {
		st := models.MockState{Name: n, Kind: models.HTTP, Usage: models.Deleted, IsFiltered: true, Lifetime: models.LifetimePerTest}
		if a.learnedOnce && n == "poll-2" && a.opens["test-2"] == 1 {
			st.ReqBodyNoise = map[string][]string{"body.at": {}}
		}
		if a.late[n] {
			st.ReqBodyNoise = map[string][]string{"body.at": {}}
			st.LearnedAfterEnd = true
			delete(a.late, n)
		}
		out = append(out, st)
	}
	a.pending = nil
	for _, n := range a.gap {
		out = append(out, models.MockState{Name: n, Kind: models.HTTP, Usage: models.Updated, Lifetime: models.LifetimeSession, ReqBodyNoise: map[string][]string{"body.at": {}}})
	}
	a.gap, a.later = a.later, nil
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
	// resetBAfter > 0: the app resets every request of test-2's after its
	// first resetBAfter (the retry cycles' ones), so each is filed with no
	// answer.
	resetBAfter int32
	// failAhead and failDrainAt are aheadInstr's.
	failAhead, failDrainAt string
	// learn runs the set as record's full round (config.Test.Learn).
	learn bool
	// learnedOnce is aheadInstr's; persisted, when set, is handed what the
	// full round keeps of what it learned (PersistLearned).
	learnedOnce bool
	persisted   func(map[string]models.MockState)
	// gapLearns is aheadInstr's.
	gapLearns bool
	// earlyStream makes test-3 the streaming one and test-4 a plain one:
	// test-3, recorded before test-4, runs after it, in the streaming pass.
	earlyStream bool
	// reconsumed, unreadAt, lateLearnAt and lateInStream are aheadInstr's.
	reconsumed, unreadAt, lateLearnAt string
	lateInStream                      bool
}

// aheadMockDB is prMockDB that hands what the full round keeps to persisted.
type aheadMockDB struct {
	*prMockDB
	persisted func(map[string]models.MockState)
}

func (m aheadMockDB) PersistLearned(_ context.Context, _ string, states map[string]models.MockState) (map[string][]string, error) {
	m.persisted(states)
	return nil, nil
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
	// resetsB counts a request of test-2's, and says whether the app resets it.
	resetsB := func() bool {
		if !o.resetB && o.resetBAfter == 0 {
			return false
		}
		n := resets.Add(1)
		return o.resetB && n == 1 || o.resetBAfter > 0 && n > o.resetBAfter
	}
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		for _, p := range []string{"/a", "/b", "/c"} {
			body := p[1:]
			if p == "/a" {
				body = "not a" // test-1 fails every time: the passing ones run again
			}
			path := p
			mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
				if path == "/b" && resetsB() {
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
	if o.earlyStream {
		cases[2] = tmplCase(3, app.addr, "GET", "/d", nil, "", models.HTTPResp{StatusCode: 200,
			Header: map[string]string{"Content-Type": "application/x-ndjson"}, Body: "{\"d\":1}\n"})
		cases[3] = tmplCase(4, app.addr, "GET", "/c", nil, "", recorded(200, nil, "c", nil))
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
		c.Test.Learn = o.learn
		if unsendable != "" {
			c.Test.BasePath = "http://" + app.addr
		}
	})
	mocks := []*models.Mock{poll(2), poll(3), poll(4), poll(5)}
	h.r.mockDB.(*prMockDB).filtered = mocks
	if o.persisted != nil {
		h.r.mockDB = aheadMockDB{prMockDB: h.r.mockDB.(*prMockDB), persisted: o.persisted}
	}
	entry := func(m *models.Mock) models.MockEntry {
		return models.MockEntry{Name: m.Name, Kind: string(m.Kind), ReqTimestampMock: models.FormatMockTimestamp(m.Spec.ReqTimestampMock)}
	}
	h.r.mappingDB.(*prMappingDB).onDisk = map[string][]models.MockEntry{
		"test-1": {}, "test-2": {entry(mocks[0])}, "test-3": {entry(mocks[1])}, "test-4": {entry(mocks[2])}, "test-5": {entry(mocks[3])},
	}
	instr := &aheadInstr{prInstr: h.r.instrumentation.(*prInstr), owned: map[string][]string{"test-2": {"poll-2"}, "test-3": {"poll-3"}, "test-4": {"poll-4"}, "test-5": {"poll-5"}}}
	instr.cases = cases
	instr.failAhead, instr.failDrainAt = o.failAhead, o.failDrainAt
	instr.learnedOnce = o.learnedOnce
	instr.gapLearns = o.gapLearns
	instr.reconsumed, instr.unreadAt, instr.lateLearnAt, instr.lateInStream = o.reconsumed, o.unreadAt, o.lateLearnAt, o.lateInStream
	h.r.instrumentation = instr
	h.r.hookImpl = newHooks(prLogger(), h.r.config, instr, false)
	h.run(t)
	// With a failed read what test-2 consumed is not all known: it has no
	// verdict to check here.
	switch r := h.result(t, "test-2"); {
	case o.failAhead != "" || o.failDrainAt != "" || o.unreadAt == "test-2" || o.lateLearnAt == "test-2":
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

// Record's full round learns in a set's first cycle only: every filter of a
// retry cycle, and of the streaming pass after it, ends the agent's learn step
// (models.MockFilterParams.LearnEnded), as those run the tests out of their
// recorded order; every filter of the first cycle, and of a set with no retry
// cycle, leaves it learning. keploy test, which learns nothing, never says it.
func TestRunTestSet_ARetryCycleOfRecordsFullRoundEndsTheLearnStep(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name         string
		retry, learn bool
		wantEnded    string
	}{
		{"record's full round with retry cycles", true, true, "pool poll-2"},
		{"record's full round without retry cycles", false, true, ""},
		{"keploy test with retry cycles", true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, instr := aheadRun(t, aheadOpts{retry: tc.retry, learn: tc.learn})
			instr.mu.Lock()
			defer instr.mu.Unlock()
			var exact []models.MockFilterParams
			for _, p := range instr.allParams {
				if p.Exact != nil && !p.Exact.Stage {
					exact = append(exact, p)
				}
			}
			sends := exactSends(exact)
			first := len(exact)
			for i, p := range exact {
				if p.LearnEnded {
					first = i
					break
				}
			}
			for i, p := range exact[first:] {
				if !p.LearnEnded {
					t.Fatalf("%q, after the learn step ended, takes it back up: %v", sends[first+i], sends)
				}
			}
			switch {
			case tc.wantEnded == "" && first != len(exact):
				t.Fatalf("the learn step ended at %q: %v", sends[first], sends)
			case tc.wantEnded != "":
				// It ends after the first cycle's last test case, and
				// before the retry cycle's first opens (test-2's second).
				retryOpen, lastFirst, test2 := -1, -1, 0
				for i, s := range sends {
					if s == "open test-2" {
						if test2++; test2 == 2 {
							retryOpen = i
							break
						}
					}
					if strings.HasPrefix(s, "open ") {
						lastFirst = i
					}
				}
				if first == len(exact) || sends[first] != tc.wantEnded || retryOpen < 0 || first <= lastFirst || first >= retryOpen {
					t.Fatalf("the learn step ends at send %d; want %q, after the first cycle's last test case (send %d) and before the retry cycle's first opens (send %d): %v", first, tc.wantEnded, lastFirst, retryOpen, sends)
				}
			}
			for _, p := range instr.allParams {
				if p.Exact != nil && p.Exact.Stage && p.LearnEnded {
					t.Fatal("the set's staging ends the learn step")
				}
			}
		})
	}
}

// A test that consumed, after the set's learn step ended, a mock its parser
// learned on all the same (MockState.LearnedAfterEnd: a parser that does not
// read the end) has no verdict, and its row says why: keploy keeps nothing
// learned then, and its run leaned on it. So in a retry cycle (test-2) and
// in the streaming pass after them (test-4, which fails here, and keeps
// that verdict), read before its answer's body or after it is read whole; a
// test whose consumes learned nothing then keeps its verdict.
func TestRunTestSet_AConsumeThatLearnedAfterTheLearnStepEndedLeavesItsTestWithNoVerdict(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		late     string
		inStream bool
		// noAnswer: the retry cycle's request gets no answer, so the row
		// is filed early (FAILED), with what the test consumed read first.
		noAnswer bool
	}{{"test-2", false, false}, {"test-2", false, true}, {"test-4", false, false}, {"test-4", true, false}} {
		late := tc.late
		t.Run(fmt.Sprintf("%s, read after its body: %v, no answer: %v", late, tc.inStream, tc.noAnswer), func(t *testing.T) {
			o := aheadOpts{retry: true, learn: true, learnedOnce: true, lateLearnAt: late, lateInStream: tc.inStream, persisted: func(map[string]models.MockState) {}}
			if tc.noAnswer {
				o.resetBAfter = 1
			}
			_, h, instr := aheadRun(t, o)
			if tc.inStream {
				instr.mu.Lock()
				held := len(instr.heldLate)
				instr.mu.Unlock()
				if held != 0 {
					t.Fatalf("precondition: %s's late consumes were never read", late)
				}
			}
			if instr.opens[late] < 2 && late == "test-2" {
				t.Fatalf("precondition: %s opened %d times; a retry cycle opens it again", late, instr.opens[late])
			}
			// The set's report: its rows are the last cycle's, with what
			// the set's end marks on them.
			final := func(name string) models.TestResult {
				t.Helper()
				h.report.mu.Lock()
				defer h.report.mu.Unlock()
				if h.report.report != nil {
					for _, r := range h.report.report.Tests {
						if r.TestCaseID == name {
							return r
						}
					}
				}
				t.Fatalf("no row for %s in the set's report", name)
				return models.TestResult{}
			}
			r := final(late)
			if !strings.Contains(r.FailureInfo.LearningNotKept, "after the test set's learn step ended") || !strings.Contains(r.FailureInfo.LearningNotKept, "poll-"+late[len(late)-1:]) {
				t.Fatalf("%s's row does not say a parser learned on poll-%s after the learn step ended: %q", late, late[len(late)-1:], r.FailureInfo.LearningNotKept)
			}
			// A FAILED row keeps its verdict, which no learning made.
			if r.Status != models.TestStatusFailed && len(r.VerdictUnknown()) == 0 {
				t.Fatalf("%s has a verdict (%s) its run leaned on unkept learning for", late, r.Status)
			}
			if other := final("test-3"); strings.Contains(other.FailureInfo.LearningNotKept, "learn step ended") || len(other.VerdictUnknown()) > 0 {
				t.Fatalf("test-3, whose consumes learned nothing after the end, lost its verdict: %+v", other.FailureInfo)
			}
			// The set, whose other test cases fail here (aheadRun's test-1),
			// keeps FAILED beside it, with the note
			// (TestRunTestSet_ASetWhoseTestLearnedAfterTheEndIsNotJudgedPassed
			// pins a set with no FAILED test).
			h.report.mu.Lock()
			setStatus, failed := h.report.report.Status, slices.ContainsFunc(h.report.report.Tests, func(r models.TestResult) bool { return r.Status == models.TestStatusFailed })
			h.report.mu.Unlock()
			if want := models.TestSetStatusFailed; !failed || setStatus != string(want) {
				t.Fatalf("the set is %s beside a FAILED test (%v); want %s", setStatus, failed, want)
			}
		})
	}
}

// A set has no verdict its tests do not give: beside a test that consumed a
// mock a parser learned on after the set's learn step ended
// (MockState.LearnedAfterEnd), whose run leaned on learning keploy does not
// keep, a set with no FAILED test is not judged (INTERNAL_ERR), never PASSED,
// and the run goes on to the next set; one with a FAILED test keeps FAILED.
func TestRunTestSet_ASetWhoseTestLearnedAfterTheEndIsNotJudgedPassed(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprintf("another reason fails it: %v", failing), func(t *testing.T) {
			setSwitch(t, true)
			h := newPartialRunHarness(t, 1, 0)
			mappedTo(h, sessionTierHTTP("mock-1"))
			hooks := prHooks{consumed: []models.MockState{{Name: "mock-1", Kind: models.HTTP, Type: "HTTP_CLIENT", Lifetime: models.LifetimeSession, LearnedAfterEnd: true}}}
			if failing {
				hooks.wrongBody = map[string]bool{"test-1": true}
			}
			h.replayer.hookImpl = hooks
			status := h.run(t)
			row := resultOf(t, h, "test-1")
			if !strings.Contains(row.FailureInfo.LearningNotKept, "after the test set's learn step ended") {
				t.Fatalf("test-1's row does not say a parser learned on mock-1 after the learn step ended: %q", row.FailureInfo.LearningNotKept)
			}
			if failing {
				if status != models.TestSetStatusFailed || row.Status != models.TestStatusFailed {
					t.Fatalf("set %s, test-1 %s; a set whose test FAILED keeps FAILED", status, row.Status)
				}
				return
			}
			if status != models.TestSetStatusInternalErr || !h.replayer.notJudged("test-set-0") {
				t.Fatalf("set %s (not judged: %v); want INTERNAL_ERR, not judged, beside a test with no verdict", status, h.replayer.notJudged("test-set-0"))
			}
			if reason := h.report.report.FailureReason; !strings.Contains(reason, "not judged") || !strings.Contains(reason, "test-1") {
				t.Fatalf("failure reason %q does not say the set is not judged, and for which test", reason)
			}
			if abortAfterTestSet(status, utils.Native, true) {
				t.Fatal("an unjudged set must let the run go on to the next set")
			}
		})
	}
}

// What a set that ran retry cycles keeps is what its first cycle learned: a
// later cycle's state of the same mock carries nothing learned (its learn
// step ended), and must not take the first cycle's place. Without retry
// cycles the one cycle's learning is kept as it is.
func TestRunTestSet_ARetryCycleKeepsTheFirstCyclesLearning(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, retry := range []bool{true, false} {
		t.Run(fmt.Sprintf("retry=%v", retry), func(t *testing.T) {
			var kept map[string]models.MockState
			calls := 0
			_, _, instr := aheadRun(t, aheadOpts{retry: retry, learn: true, learnedOnce: true, persisted: func(s map[string]models.MockState) {
				calls++
				kept = s
			}})
			if calls != 1 {
				t.Fatalf("the full round kept its learning %d times; once", calls)
			}
			instr.mu.Lock()
			opens := instr.opens["test-2"]
			instr.mu.Unlock()
			if retry && opens < 2 {
				t.Fatalf("precondition: test-2 opened %d times; the retry cycles open it again", opens)
			}
			st, ok := kept["poll-2"]
			if !ok {
				t.Fatalf("poll-2 is not among what the round kept: %v", kept)
			}
			if _, ok := st.ReqBodyNoise["body.at"]; !ok {
				t.Fatalf("what the first cycle learned of poll-2 is not kept (a later cycle's state took its place): %+v", st)
			}
		})
	}
}

// What the first cycle's learn step learned after its last test case, in the
// gap before the retry cycle's first (a heartbeat the pool answers), is the
// first cycle's, and kept: the learn step ends on the agent, and that gap is
// read, before what the first cycle learned is taken. Read only with the
// retry cycle's first test case, it would be read after, and dropped.
func TestRunTestSet_WhatTheFirstCycleLearnedAfterItsLastTestIsKept(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	var kept map[string]models.MockState
	_, _, instr := aheadRun(t, aheadOpts{retry: true, learn: true, learnedOnce: true, gapLearns: true, persisted: func(s map[string]models.MockState) {
		kept = s
	}})
	instr.mu.Lock()
	last := fmt.Sprintf("hb-%d", instr.poolCalls)
	opens := instr.opens["test-2"]
	events := append([]string(nil), instr.events...)
	instr.mu.Unlock()
	if opens < 2 || last == "hb-0" {
		t.Fatalf("precondition: test-2 opened %d times and the first cycle served the pool (%s); the retry cycles open it again", opens, last)
	}
	// The gap is read after a pool call that ended the learn step, and
	// before the retry cycle's first test case opens: what the learn step
	// learned of the gap's calls is decided by then.
	ended := slices.Index(events, "learn-ended")
	retryOpen := -1
	for i, n := 0, 0; i < len(events); i++ {
		if events[i] == "test:test-2" {
			if n++; n == 2 {
				retryOpen = i
				break
			}
		}
	}
	if ended < 0 || retryOpen < ended || !slices.Contains(events[ended:retryOpen], "consumed") {
		t.Fatalf("what the gap consumed is not read between the pool call that ended the learn step (event %d) and the retry cycle's first opening (event %d): %v", ended, retryOpen, events)
	}
	for k := 1; ; k++ {
		name := fmt.Sprintf("hb-%d", k)
		st, ok := kept[name]
		if !ok || len(st.ReqBodyNoise) == 0 {
			t.Fatalf("what the first cycle learned of %s, between its test cases, is not kept: %+v", name, st)
		}
		if name == last {
			break
		}
	}
}

// Record's full round runs a set's streaming test cases after its others (the
// deferred streaming pass). One recorded before a test case that runs ahead of
// it (test-3, recorded before test-4) so runs out of its recorded order: a
// late call of test-4 read in its window would be paired with test-3's own
// recording, which test-4's call was recorded after, and its drift learned.
// Its learn step ends before it opens, as a retry cycle's does
// (models.MockFilterParams.LearnEnded): the pool call ahead of it, after the
// set's last other test case, ends it, every filter after says so, and what
// was learned before, the heartbeats after the last other test case
// included, is kept, never a later state of the same mock (hb-1 consumed
// again in the pass, learning nothing). Streaming test cases recorded after every other (test-4
// and test-5 of aheadRun's own set) run in their recorded order, and learn.
func TestRunTestSet_AStreamingTestCaseRunOutOfItsRecordedOrderLearnsNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, early := range []bool{true, false} {
		t.Run(fmt.Sprintf("recordedBeforeAnother=%v", early), func(t *testing.T) {
			var kept map[string]models.MockState
			_, _, instr := aheadRun(t, aheadOpts{learn: true, learnedOnce: true, gapLearns: true, earlyStream: early, reconsumed: "hb-1", persisted: func(s map[string]models.MockState) {
				kept = s
			}})
			instr.mu.Lock()
			var exact []models.MockFilterParams
			for _, p := range instr.allParams {
				if p.Exact != nil && !p.Exact.Stage {
					exact = append(exact, p)
				}
			}
			last := fmt.Sprintf("hb-%d", instr.poolCalls)
			instr.mu.Unlock()
			sends := exactSends(exact)
			first := slices.IndexFunc(exact, func(p models.MockFilterParams) bool { return p.LearnEnded })
			if !early {
				if first >= 0 {
					t.Fatalf("streaming test cases recorded after every other end the learn step at %q: %v", sends[first], sends)
				}
				return
			}
			lastOther, stream := slices.Index(sends, "open test-4"), slices.Index(sends, "open test-3")
			if lastOther < 0 || stream < lastOther {
				t.Fatalf("precondition: test-4 opens before test-3, the streaming one: %v", sends)
			}
			if first < 0 || first <= lastOther || first >= stream || !strings.HasPrefix(sends[first], "pool") {
				t.Fatalf("the learn step ends at send %d; want the pool call after test-4 (send %d) and before test-3 opens (send %d): %v", first, lastOther, stream, sends)
			}
			for i, p := range exact[first:] {
				if !p.LearnEnded {
					t.Fatalf("%q, after the learn step ended, takes it back up: %v", sends[first+i], sends)
				}
			}
			if st := kept["poll-2"]; len(st.ReqBodyNoise) == 0 {
				t.Fatalf("what test-2 learned is not kept: %+v", st)
			}
			if last == "hb-0" {
				t.Fatal("precondition: the set's other test cases served the pool with the learn step on")
			}
			for k := 1; ; k++ {
				name := fmt.Sprintf("hb-%d", k)
				if st := kept[name]; len(st.ReqBodyNoise) == 0 {
					t.Fatalf("what was learned of %s, before the streaming pass, is not kept: %+v", name, st)
				}
				if name == last {
					break
				}
			}
		})
	}
}

// keploy's replay begins every test case over the pool on each path the plain
// replay serves a test case on: keploy test's main loop, its retry cycles
// and its streaming pass, record's full round's retry cycles (its learn step
// ended), and a streaming pass run out of recorded order. The agent's learn
// tracker begins a test case as its capture window opens, and the agent
// installs a test's own view only as the replay asks: so its store never
// shows test N-1's own view while the tracker has test N open (the state of
// integrations.RoundSnapshot.TrackerAhead in which a learn step waits). HTTP's
// matcher, which on those paths serves exactly from the store's view without
// reading the tracker, so never answers a call read after test N began from
// test N-1's own view: from the tracker's begin to N's install the store
// shows the pool, and the call is the pool's (RoundSnapshot.PoolsCall); the
// application gets test N's request only after the install.
func TestEveryTestCaseBeginsOverThePoolOnThePlainReplaysPaths(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name               string
		learn, earlyStream bool
	}{
		{"keploy test", false, false},
		{"keploy test, a streaming test case recorded before another", false, true},
		{"record's full round", true, false},
		{"record's full round, a streaming test case recorded before another", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, instr := aheadRun(t, aheadOpts{retry: true, learn: tc.learn, earlyStream: tc.earlyStream})
			instr.mu.Lock()
			events := append([]string(nil), instr.events...)
			opens := instr.opens["test-2"]
			instr.mu.Unlock()
			if opens < 2 || !slices.Contains(events, "test:test-5") || !slices.Contains(events, "continue") {
				t.Fatalf("precondition: the retry cycles opened test-2 again (%d opens) and the streaming pass ran, its test cases each begun: %v", opens, events)
			}
			if bad := overATestView(events); bad != "" {
				t.Fatalf("a test case began while the agent may still serve a test's own view, %s: %v", bad, events)
			}
		})
	}
}

// A set's retry cycles are served what its first cycle learned: one filter,
// the pool's with the learn step ended, after the gap after the first
// cycle's last test case is read and before the retry cycle's first test
// case opens, carries exactly the learning the round keeps
// (models.MockFilterParams.Learned), and none other does. A set with no
// retry cycle whose streaming test case was recorded before another (test-3,
// before test-4) ends its learn step before its deferred streaming pass, and
// is served it the same way, before that test case opens: with the learn
// step ended the agent hands its staged recordings out as staged, so without
// it the pass would be judged with nothing learned. Without either no filter
// carries any.
func TestRunTestSet_TheRetryCyclesAreServedWhatTheFirstCycleLearned(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		retry       bool
		unread      string
		earlyStream bool
	}{{true, "", false}, {false, "", false}, {true, "test-2", false}, {false, "", true}} {
		retry := tc.retry
		t.Run(fmt.Sprintf("retry=%v,unread=%q,streamingRecordedBeforeAnother=%v", retry, tc.unread, tc.earlyStream), func(t *testing.T) {
			var kept map[string]models.MockState
			o := aheadOpts{retry: retry, learn: true, learnedOnce: true, gapLearns: true, unreadAt: tc.unread, earlyStream: tc.earlyStream, persisted: func(s map[string]models.MockState) {
				kept = s
			}}
			if tc.earlyStream {
				// The pass consumes a heartbeat again, learning nothing:
				// what it is served is what was learned before.
				o.reconsumed = "hb-1"
			}
			_, _, instr := aheadRun(t, o)
			instr.mu.Lock()
			sends := append([]models.MockFilterParams(nil), instr.allParams...)
			instr.mu.Unlock()
			at, ended, reopen := -1, -1, -1
			opened := map[string]bool{}
			for i, p := range sends {
				if len(p.Learned) > 0 {
					if at >= 0 {
						t.Fatalf("filters %d and %d both carry the first cycle's learning; once", at, i)
					}
					at = i
				}
				if p.LearnEnded && ended < 0 {
					ended = i
				}
				if p.Exact != nil && p.Exact.Test != "" {
					if opened[p.Exact.Test] && reopen < 0 {
						reopen = i
					}
					opened[p.Exact.Test] = true
				}
			}
			if !retry && !tc.earlyStream {
				if at >= 0 {
					t.Fatalf("a set with no retry cycle, its streaming test cases recorded after the others, was sent learning on filter %d: %+v", at, sends[at].Learned)
				}
				return
			}
			if at < 0 {
				t.Fatal("no filter served the test cases run with the learn step ended what the set learned before")
			}
			p := sends[at]
			if !p.LearnEnded || p.Exact == nil || p.Exact.Test != "" || p.Exact.Stage {
				t.Fatalf("the learning came on filter %d, not the pool's with the learn step ended: %+v", at, p)
			}
			if !retry {
				// The deferred streaming pass's test case opens for the
				// first time, after the learning.
				reopen = slices.IndexFunc(sends, func(p models.MockFilterParams) bool { return p.Exact != nil && p.Exact.Test == "test-3" })
				if before := slices.IndexFunc(sends, func(p models.MockFilterParams) bool { return p.Exact != nil && p.Exact.Test == "test-4" }); before < 0 || reopen < before {
					t.Fatalf("precondition: test-4 opens before test-3, the streaming one (%d, %d)", before, reopen)
				}
			}
			if !(ended < at && at < reopen) {
				t.Fatalf("the learning came on filter %d; after the learn step ended (%d) and before the first opening it ends it for (%d)", at, ended, reopen)
			}
			var want, got []string
			for n, st := range kept {
				if len(st.ReqBodyNoise) > 0 || len(st.Variables) > 0 || len(st.Binds) > 0 {
					want = append(want, n)
				}
			}
			for _, st := range p.Learned {
				got = append(got, st.Name)
				// What the round keeps carries where each decision came
				// from (learn.StampStates), which serving never reads.
				if !reflect.DeepEqual(decisions(st), decisions(kept[st.Name])) {
					t.Fatalf("%s was served the retry cycles as %+v; the round keeps %+v", st.Name, st, kept[st.Name])
				}
			}
			sort.Strings(want)
			// What the agent learned for test-2, whose request it did not
			// say it read, is not kept, and not served (withoutUnreadLearning).
			if tc.unread != "" {
				if !reflect.DeepEqual(got, want) || slices.Contains(got, "poll-2") || len(got) == 0 {
					t.Fatalf("the retry cycles were served the learning of %v; the round keeps %v (heartbeats the gap learned, and not poll-2's, which test-2 learned with its request unread)", got, want)
				}
				return
			}
			if !reflect.DeepEqual(got, want) || !slices.Contains(got, "poll-2") || !slices.Contains(got, "hb-1") {
				t.Fatalf("the retry cycles were served the learning of %v; the round keeps %v (poll-2's and the gap's heartbeats among it)", got, want)
			}
		})
	}
}
