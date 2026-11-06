package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// fullRoundHost is the dependency the rows below call, as the application
// names it, and the address the proxy dials for it.
const fullRoundHost, fullRoundAddr = "pay:8080", "127.0.0.1:50071"

// onPay is a recording of a call to fullRoundHost, the Host it was sent with
// in its headers and its answer's length in the answer's, as a recorded HTTP
// mock holds them.
func onPay(m *models.Mock) *models.Mock {
	m.Spec.HTTPReq.URL = "http://" + fullRoundHost + m.Spec.HTTPReq.URL
	m.Spec.HTTPReq.Header["Host"] = fullRoundHost
	m.Spec.HTTPResp.Header["Content-Length"] = strconv.Itoa(len(m.Spec.HTTPResp.Body))
	return m
}

// served is what the application got for one call, and what keploy reported
// of it.
type served struct {
	status   int
	body     string
	reported []error
}

// serveCall sends one call of the application through the HTTP parser on a
// connection of its own, in record's full round (learnOn) or a replay, and
// returns the answer.
func serveCall(t *testing.T, ctx context.Context, db integrations.MockMemDb, learnOn bool, method, path, body string) served {
	t.Helper()
	return serveCallWith(t, ctx, db, models.OutgoingOptions{Learn: learnOn}, method, path, body)
}

// serveCallWith is serveCall under opts (opts.Learn: record's full round),
// the call sending headers too, each a "Name: value" line.
func serveCallWith(t *testing.T, ctx context.Context, db integrations.MockMemDb, opts models.OutgoingOptions, method, path, body string, headers ...string) served {
	t.Helper()
	var got served
	ctx = models.WithMockMismatchReporter(ctx, func(err error) { got.reported = append(got.reported, err) })
	app, prox := net.Pipe()
	defer func() { _ = app.Close() }()
	done := make(chan error, 1)
	go func() {
		done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: fullRoundAddr}, db, opts)
	}()
	wire := method + " " + path + " HTTP/1.1\r\nHost: " + fullRoundHost + "\r\nContent-Type: application/json\r\n"
	for _, h := range headers {
		wire += h + "\r\n"
	}
	if body != "" {
		wire += "Content-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	// An answer the parser never finishes fails the row, never hangs it.
	_ = app.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(app, wire+"\r\n"+body); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the app got no whole answer: %v", err)
	}
	_ = resp.Body.Close()
	_ = app.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the parser did not return")
	}
	got.status, got.body = resp.StatusCode, string(b)
	return got
}

// openTest is a context whose learn tracker has a test open, as record's
// full round holds one while a test case runs.
func openTest() context.Context {
	tr := learn.NewTracker()
	tr.Begin()
	return learn.WithTracker(context.Background(), tr)
}

// A recording that declares variables is answered in record's full round as
// a replay answers it: by the variable pass. A derived mock that keeps its
// source's echo variable answers a call with a new key, echoing that key; a
// kept recording whose learned variable is bound at one call and used at the
// next answers both calls of a run with new values.
func TestFullRoundServesARecordingThatDeclaresVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	echo := func() *varDb {
		declined := at(onPay(varMock("mock-1", "POST", "/charge", `{"key":"k-rec","amount":5}`, 402, `{"key":"k-rec","declined":true}`,
			models.MockVariables{"key": {Recorded: "k-rec", Bind: "body.key", Answer: []string{"body.key"}, Echo: true}})), 1)
		return &varDb{perTest: []*models.Mock{declined}}
	}
	bound := func() *varDb {
		create := at(onPay(varMock("mock-1", "POST", "/orders", `{"id":"r1"}`, 201, `{"created":true}`,
			models.MockVariables{"orderId": bindV("r1", "body.id")})), 1)
		read := at(onPay(varMock("mock-2", "GET", "/orders/r1", "", 200, `{"state":"paid"}`,
			models.MockVariables{"orderId": useV("r1", []string{"url.path[1]"})})), 2)
		return &varDb{perTest: []*models.Mock{create, read}}
	}
	for _, learnOn := range []bool{false, true} {
		round := map[bool]string{true: "record's full round", false: "a replay"}[learnOn]
		db := echo()
		got := serveCall(t, openTest(), db, learnOn, "POST", "/charge", `{"key":"k-new","amount":5}`)
		if got.status != 402 || got.body != `{"key":"k-new","declined":true}` {
			t.Fatalf("in %s a charge with a new key got %d %q (reports %v); want the derived decline echoing k-new", round, got.status, got.body, got.reported)
		}
		if !db.consumed["mock-1"] {
			t.Fatalf("in %s the decline answered without being taken from the store", round)
		}

		db, ctx := bound(), openTest()
		if got := serveCall(t, ctx, db, learnOn, "POST", "/orders", `{"id":"l1"}`); got.status != 201 {
			t.Fatalf("in %s the create got %d %q (reports %v); want mock-1's answer", round, got.status, got.body, got.reported)
		}
		if got := serveCall(t, ctx, db, learnOn, "GET", "/orders/l1", ""); got.status != 200 || got.body != `{"state":"paid"}` {
			t.Fatalf("in %s the read of the created order got %d %q (reports %v); want mock-2's answer", round, got.status, got.body, got.reported)
		}
	}
}

// reportOf is the miss report keploy reported of a call that missed.
func reportOf(t *testing.T, got served) *models.MockMismatchReport {
	t.Helper()
	var withReport interface {
		MismatchReport() *models.MockMismatchReport
	}
	if len(got.reported) != 1 || !errors.As(got.reported[0], &withReport) {
		t.Fatalf("reported %v; want one miss with its report", got.reported)
	}
	return withReport.MismatchReport()
}

// In record's full round a call no recording that declares variables fits is
// still the learn step's: it is answered from the recording it drifted from,
// which a replay's matcher, before the learn step learned the drift, misses.
// A call a recording that declares variables fits but whose variables do
// not hold misses with that evidence in both, as a replay misses it: the
// learn step never pairs it with another recording.
func TestFullRoundLeavesTheLearnStepItsCalls(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, learnOn := range []bool{false, true} {
		round := map[bool]string{true: "record's full round", false: "a replay"}[learnOn]
		declined := at(onPay(varMock("mock-1", "POST", "/charge", `{"key":"k-rec","amount":5}`, 402, `{"key":"k-rec","declined":true}`,
			models.MockVariables{"key": {Recorded: "k-rec", Bind: "body.key", Answer: []string{"body.key"}, Echo: true}})), 1)
		audit := at(onPay(varMock("mock-2", "POST", "/audit", `{"note":"n-rec"}`, 200, `{"logged":true}`, nil)), 2)
		db, ctx := &varDb{perTest: []*models.Mock{declined, audit}}, openTest()
		if got := serveCall(t, ctx, db, learnOn, "POST", "/charge", `{"key":"k-new","amount":5}`); got.status != 402 {
			t.Fatalf("in %s the charge got %d %q (reports %v); want the decline", round, got.status, got.body, got.reported)
		}
		got := serveCall(t, ctx, db, learnOn, "POST", "/audit", `{"note":"n-new"}`)
		if learnOn && (got.status != 200 || got.body != `{"logged":true}` || !db.consumed["mock-2"]) {
			t.Fatalf("in %s the drifted audit call got %d %q (reports %v); want mock-2's answer, the drift learned", round, got.status, got.body, got.reported)
		}
		if !learnOn && got.status != http.StatusBadGateway {
			t.Fatalf("in %s the drifted audit call got %d %q; want a miss: nothing learned its drift", round, got.status, got.body)
		}

		create := at(onPay(varMock("mock-1", "POST", "/orders", `{"id":"r1"}`, 201, `{"created":true}`,
			models.MockVariables{"orderId": bindV("r1", "body.id")})), 1)
		read := at(onPay(varMock("mock-2", "GET", "/orders/r1", "", 200, `{"state":"paid"}`,
			models.MockVariables{"orderId": useV("r1", []string{"url.path[1]"})})), 2)
		db, ctx = &varDb{perTest: []*models.Mock{create, read}}, openTest()
		if got := serveCall(t, ctx, db, learnOn, "POST", "/orders", `{"id":"l1"}`); got.status != 201 {
			t.Fatalf("in %s the create got %d %q (reports %v); want mock-1's answer", round, got.status, got.body, got.reported)
		}
		got = serveCall(t, ctx, db, learnOn, "GET", "/orders/zz", "")
		if got.status != http.StatusBadGateway {
			t.Fatalf("in %s a read of an order the test never created got %d %q; want a miss", round, got.status, got.body)
		}
		if r := reportOf(t, got); r.MatchPhase != models.MatchPhaseVariable || r.ClosestMock != "mock-2" {
			t.Fatalf("in %s the read's miss is %+v; want mock-2's variable miss", round, r)
		}
		if db.consumed["mock-2"] {
			t.Fatalf("in %s the read that missed took mock-2", round)
		}
	}
}

// roundVarDb is varDb read in one snapshot of the round
// (integrations.RoundReader), as the agent's MockManager is under exact
// serving: the store shows test-1's view, its per-test tier test-1's own,
// while the learn tracker the query names has a test open. ended, where
// set, is a test the store ended before the tracker did, its view no longer
// shown (integrations.RoundSnapshot.NoOneRound). held, where set, is the
// store held to a call's round (integrations.RoundHolder): a view that
// reads nothing once the replay staged another set after the call's first
// read, where the store itself shows that set's recordings.
type roundVarDb struct {
	*varDb
	ended string
	held  *varDb
	// readErr: a read of the store's per-test tier, other than the round's
	// snapshot, fails.
	readErr bool
}

func (d *roundVarDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	if d.readErr {
		return nil, errors.New("the store could not be read")
	}
	return d.varDb.GetPerTestMocksInWindow()
}

func (d *roundVarDb) RoundSnapshot(q integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	s := integrations.RoundSnapshot{At: integrations.RoundAt{Epoch: 1, Gen: 1}, Phase: integrations.RoundInTest, Test: "test-1", PerTest: d.perTest, Session: d.session}
	if t := q.Tracker; t != nil {
		p := t.Placed()
		s.Learn = integrations.LearnRead{Bound: true, Number: p.Test, Open: p.Open, Begun: p.Begun}
	}
	owners := map[string]integrations.RoundOwner{}
	if d.ended != "" {
		s.Phase, s.Test, s.Ended = integrations.RoundBoundary, "", d.ended
	} else {
		for _, m := range d.perTest {
			owners[m.Name] = integrations.OwnerTest
		}
	}
	s.Open = s.Phase == integrations.RoundInTest
	return s.WithParts(integrations.RoundParts{Owners: owners}), true
}

func (d *roundVarDb) WithinRound(integrations.RoundAt) integrations.RoundScope {
	return roundVarScope{d}
}

func (*roundVarDb) ClaimPairingIn(*models.Mock, integrations.RoundAt) (claimed, moved bool, why string) {
	return false, false, "no pairing in this store"
}

func (*roundVarDb) SpendByKindIn(*models.Mock, integrations.RoundAt) (spent, moved bool, why string) {
	return false, false, "no background spend in this store"
}

func (*roundVarDb) TakenLog(uint64, int, int) []integrations.Take { return nil }

func (d *roundVarDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	if d.held != nil {
		return d.held, func() bool { return true }
	}
	return d, func() bool { return false }
}

// roundVarScope is roundVarDb as a RoundScope whose round never moves.
type roundVarScope struct{ *roundVarDb }

func (roundVarScope) Moved() string { return "" }

var (
	_ integrations.RoundReader = (*roundVarDb)(nil)
	_ integrations.RoundHolder = (*roundVarDb)(nil)
)

// In record's full round the variable pass decides a call in the round the
// learn step's first pass read it in, and in none other: a call of no one
// round (the store ended a test its learn tracker still has open) stays the
// learn step's moved miss, never answered by a recording of the view the
// store shows; and a call whose round the replay staged another set over is
// answered by no recording of that set, its store held to the call's round
// reading none.
func TestFullRoundVariablesDecideACallInItsOwnRound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	declined := func() *models.Mock {
		return at(onPay(varMock("mock-1", "POST", "/charge", `{"key":"k-rec","amount":5}`, 402, `{"key":"k-rec","declined":true}`,
			models.MockVariables{"key": {Recorded: "k-rec", Bind: "body.key", Answer: []string{"body.key"}, Echo: true}})), 1)
	}
	for _, c := range []struct {
		name string
		db   *roundVarDb
		// why is how the miss's next steps begin.
		why string
	}{
		{"the store ended test-1 before the learn tracker did", &roundVarDb{varDb: &varDb{session: []*models.Mock{declined()}}, ended: "test-1"},
			"keploy record's full round read this request after keploy's store ended test-1 and before its learn tracker did"},
		{"the replay staged another set after the call's first read", &roundVarDb{varDb: &varDb{perTest: []*models.Mock{declined()}}, held: &varDb{}},
			"keploy's learn step: no recorded call has its method, destination and shape"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := serveCall(t, openTest(), c.db, true, "POST", "/charge", `{"key":"k-new","amount":5}`)
			if got.status != http.StatusBadGateway {
				t.Fatalf("the charge got %d %q (reports %v); want a miss: the decline is of no round the call was made in", got.status, got.body, got.reported)
			}
			if r := reportOf(t, got); r.MatchPhase != models.MatchPhaseExhausted || !strings.HasPrefix(r.NextSteps, c.why) {
				t.Fatalf("the charge's miss is %+v; want the learn step's, beginning %q", r, c.why)
			}
			if c.db.consumed["mock-1"] || len(c.db.perTest)+len(c.db.session) != 1 {
				t.Fatalf("the charge that missed took or changed the decline (taken %v)", c.db.consumed)
			}
		})
	}
}

// ownHeldVarDb is roundVarDb under the background rules whose call's hold
// (integrations.RoundHolder) is hold, a store of its own, and whose own
// reads tell, unheld, the open test's own recordings owned
// (integrations.SharedReads; none: it tells none).
type ownHeldVarDb struct {
	*roundVarDb
	hold  *ownHold
	owned []*models.Mock
}

func (d *ownHeldVarDb) RoundSnapshot(q integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	s, ok := d.roundVarDb.RoundSnapshot(q)
	s.Background = true
	return s, ok
}

func (d *ownHeldVarDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d.hold, func() bool { return false }
}

func (d *ownHeldVarDb) SharedGeneration(models.Kind) (integrations.SharedGen, bool) {
	return integrations.SharedGen{}, false
}
func (d *ownHeldVarDb) SharedMocks() (perTest, session []*models.Mock, ok bool) {
	return nil, nil, false
}
func (d *ownHeldVarDb) OwnedUnanswered() ([]*models.Mock, bool) { return nil, false }
func (d *ownHeldVarDb) OwnedRecordings() (uint64, []*models.Mock, bool) {
	return fakeOwnedView(d.owned, d.owned != nil)
}
func (d *ownHeldVarDb) OwnedUnansweredAt(view uint64, at []int) ([]int, bool) {
	return fakeOwnedAt(view, at)
}
func (d *ownHeldVarDb) StagingGeneration() uint64                  { return 1 }
func (d *ownHeldVarDb) StagingGenerationByKind(models.Kind) uint64 { return 1 }
func (d *ownHeldVarDb) MockSpent(string) bool                      { return false }

// ownHold is varDb as a call's hold under the background rules: its session
// tier is the set's background, which it hides from the call once the
// call's own-call decision says the call is one of the open test's own
// (integrations.BackgroundHider: asked at the first door a background
// recording reaches); owned are the open test's own recordings, answered or
// not, as its reads tell them (integrations.SharedReads), and answered
// those that answered (integrations.ExactConsumedReader). asked counts the
// decisions asked.
type ownHold struct {
	*varDb
	owned, answered []*models.Mock
	owns            func() bool
	hides           bool
	asked           int
}

func (h *ownHold) HideBackgroundWhen(owns func() bool) integrations.MockMemDb {
	h.owns = owns
	return h
}

func (h *ownHold) HidesBackground() bool {
	if owns := h.owns; owns != nil {
		h.owns = nil
		h.asked++
		h.hides = owns()
	}
	return h.hides
}

func (h *ownHold) background(name string) bool {
	return slices.ContainsFunc(h.session, func(m *models.Mock) bool { return m.Name == name })
}

func (h *ownHold) RefusesAnswer(mk *models.Mock) bool {
	return mk != nil && h.background(mk.Name) && h.HidesBackground()
}

func (h *ownHold) GetSessionMocks() ([]*models.Mock, error) {
	if h.hides {
		return nil, nil
	}
	return h.varDb.GetSessionMocks()
}

func (h *ownHold) UpdateUnFilteredMock(old, new *models.Mock) bool {
	return !h.RefusesAnswer(old) && h.varDb.UpdateUnFilteredMock(old, new)
}

func (h *ownHold) Spent(mk *models.Mock) bool { return h.hides && h.background(mk.Name) }

func (h *ownHold) SharedGeneration(models.Kind) (integrations.SharedGen, bool) {
	return integrations.SharedGen{}, false
}
func (h *ownHold) SharedMocks() (perTest, session []*models.Mock, ok bool) { return nil, nil, false }
func (h *ownHold) OwnedUnanswered() ([]*models.Mock, bool)                 { return nil, false }
func (h *ownHold) OwnedRecordings() (uint64, []*models.Mock, bool) {
	return fakeOwnedView(h.owned, h.owned != nil)
}
func (h *ownHold) OwnedUnansweredAt(view uint64, at []int) ([]int, bool) {
	return fakeOwnedAt(view, at)
}
func (h *ownHold) StagingGeneration() uint64                  { return 1 }
func (h *ownHold) StagingGenerationByKind(models.Kind) uint64 { return 1 }
func (h *ownHold) MockSpent(name string) bool                 { return h.hides && h.background(name) }
func (h *ownHold) ConsumedTestMocks() []*models.Mock          { return h.answered }

var (
	_ integrations.BackgroundHider     = (*ownHold)(nil)
	_ integrations.SharedReads         = (*ownHold)(nil)
	_ integrations.SharedReads         = (*ownHeldVarDb)(nil)
	_ integrations.ExactConsumedReader = (*ownHold)(nil)
)

// In record's full round the variable pass's own-call decision
// (integrations.OwnRepeatRead) reads the call's one hold, as the variable
// pass and the miss read it, never the store the call is held from. The
// open test answered its own GET /items/a1, and the set's background holds
// a recording of GET /items/r1 that binds its path's second segment, so it
// fits GET /items/a1 too. Where the hold tells the test's own recording and
// the store, unheld, tells none, the call is one call more than the test
// recorded: the background never answers it, and it misses as such,
// recorded once. Where the hold tells none and the store tells the test's
// own, the call is no repeat of a recording its hold reads: the
// background's recording answers it.
func TestFullRoundsOwnCallDecisionReadsTheCallsHold(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, heldOwns := range []bool{true, false} {
		name := map[bool]string{true: "the hold tells the test's own, the store none", false: "the store tells the test's own, the hold none"}[heldOwns]
		t.Run(name, func(t *testing.T) {
			own := at(onPay(varMock("mock-own", "GET", "/items/a1", "", 200, `{"own":true}`, nil)), 1)
			bg := at(onPay(varMock("mock-bg", "GET", "/items/r1", "", 200, `{"bg":true}`, models.MockVariables{"id": bindV("r1", "url.path[1]")})), 0)
			bg.TestModeInfo.Lifetime = models.LifetimeSession
			hold := &ownHold{varDb: &varDb{session: []*models.Mock{bg}}}
			db := &ownHeldVarDb{roundVarDb: &roundVarDb{varDb: &varDb{}}, hold: hold}
			if heldOwns {
				hold.owned, hold.answered = []*models.Mock{own}, []*models.Mock{own}
			} else {
				db.owned = []*models.Mock{own}
			}
			got := serveCall(t, openTest(), db, true, "GET", "/items/a1", "")
			if hold.asked != 1 {
				t.Fatalf("the call's hold was asked the own-call decision %d times; want once", hold.asked)
			}
			if !heldOwns {
				if got.status != 200 || got.body != `{"bg":true}` {
					t.Fatalf("the call got %d %q (reports %v); want the background's answer: its hold reads no recording it repeats", got.status, got.body, got.reported)
				}
				return
			}
			if got.status == 200 {
				t.Fatalf("the call, one more than the test recorded, was answered %q by the background's recording that declares a variable", got.body)
			}
			if r := reportOf(t, got); r.MissKind != models.MissExtraCall || r.Recorded != 1 {
				t.Fatalf("the call missed as %q (recorded %d); want one call more than the test recorded, recorded 1", r.MissKind, r.Recorded)
			}
		})
	}
}

// failFirstDb is varDb whose first read of its per-test tier fails, as a
// store that could not be read once does.
type failFirstDb struct {
	*varDb
	reads int
}

func (d *failFirstDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	d.reads++
	if d.reads == 1 {
		return nil, errors.New("the store could not be read")
	}
	return d.varDb.GetPerTestMocksInWindow()
}

// endsWithError sends one call of the application through the HTTP parser,
// as serveCall does, and fails the row unless the parser ends with an error
// and the application gets no answer.
func endsWithError(t *testing.T, ctx context.Context, db integrations.MockMemDb, learnOn bool, method, path, body string) {
	t.Helper()
	app, prox := net.Pipe()
	defer func() { _ = app.Close() }()
	done := make(chan error, 1)
	go func() {
		done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: fullRoundAddr}, db, models.OutgoingOptions{Learn: learnOn})
	}()
	_ = app.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(app, method+" "+path+" HTTP/1.1\r\nHost: "+fullRoundHost+"\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body); err != nil {
		t.Fatal(err)
	}
	answered := make(chan string, 1)
	go func() {
		if resp, err := http.ReadResponse(bufio.NewReader(app), nil); err == nil {
			answered <- resp.Status
		}
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the parser ended with no error")
		}
	case s := <-answered:
		t.Fatalf("the call was answered %s; want the parser's error", s)
	case <-time.After(10 * time.Second):
		t.Fatal("the parser did not return")
	}
}

// In record's full round a call the learn step could not decide (its read
// of the store failed) ends with that error: the variable pass, which reads
// the store again, never answers it in the learn step's place.
func TestFullRoundKeepsTheLearnStepsError(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	declined := at(onPay(varMock("mock-1", "POST", "/charge", `{"key":"k-rec","amount":5}`, 402, `{"key":"k-rec","declined":true}`,
		models.MockVariables{"key": {Recorded: "k-rec", Bind: "body.key", Answer: []string{"body.key"}, Echo: true}})), 1)
	db := &failFirstDb{varDb: &varDb{perTest: []*models.Mock{declined}}}
	endsWithError(t, openTest(), db, true, "POST", "/charge", `{"key":"k-new","amount":5}`)
	if db.consumed["mock-1"] {
		t.Fatal("the charge took the decline")
	}
}

// unloadable is m with its answer kept apart from it (a spilled response)
// that cannot be loaded.
func unloadable(m *models.Mock) *models.Mock {
	m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return nil, nil, errors.New("the recorded answer could not be read")
	})
	return m
}

// A call the variable pass decides ends with the error of a read it could
// not make, in record's full round as in a replay, never with a miss: the
// store's pool it could not read, the answer of the recording that fits it,
// or the answer of the exact recording that alone may answer a call whose
// variables do not hold.
func TestTheVariablePassEndsACallWithItsError(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	declined := func() *models.Mock {
		return at(onPay(varMock("mock-1", "POST", "/charge", `{"key":"k-rec","amount":5}`, 402, `{"key":"k-rec","declined":true}`,
			models.MockVariables{"key": {Recorded: "k-rec", Bind: "body.key", Answer: []string{"body.key"}, Echo: true}})), 1)
	}
	for _, learnOn := range []bool{false, true} {
		round := map[bool]string{true: "record's full round", false: "a replay"}[learnOn]
		t.Run(round+": the answer of the recording that fits", func(t *testing.T) {
			db := &varDb{perTest: []*models.Mock{unloadable(declined())}}
			endsWithError(t, openTest(), db, learnOn, "POST", "/charge", `{"key":"k-new","amount":5}`)
			if db.consumed["mock-1"] {
				t.Fatal("the charge took the decline")
			}
		})
		t.Run(round+": the answer of the exact recording", func(t *testing.T) {
			create := at(onPay(varMock("mock-1", "POST", "/orders", `{"id":"r1"}`, 201, `{"created":true}`,
				models.MockVariables{"orderId": bindV("r1", "body.id")})), 1)
			read := at(onPay(varMock("mock-2", "GET", "/orders/r1", "", 200, `{"state":"paid"}`,
				models.MockVariables{"orderId": useV("r1", []string{"url.path[1]"})})), 2)
			exact := unloadable(at(onPay(varMock("mock-3", "GET", "/orders/zz", "", 404, `{"missing":true}`, nil)), 3))
			db, ctx := &varDb{perTest: []*models.Mock{create, read, exact}}, openTest()
			if got := serveCall(t, ctx, db, learnOn, "POST", "/orders", `{"id":"l1"}`); got.status != 201 {
				t.Fatalf("the create got %d %q (reports %v); want mock-1's answer", got.status, got.body, got.reported)
			}
			endsWithError(t, ctx, db, learnOn, "GET", "/orders/zz", "")
			if db.consumed["mock-2"] || db.consumed["mock-3"] {
				t.Fatalf("the read took a recording (taken %v)", db.consumed)
			}
		})
	}
	t.Run("record's full round: the store's pool", func(t *testing.T) {
		db := &roundVarDb{varDb: &varDb{perTest: []*models.Mock{declined()}}, readErr: true}
		endsWithError(t, openTest(), db, true, "POST", "/charge", `{"key":"k-new","amount":5}`)
		if db.consumed["mock-1"] {
			t.Fatal("the charge took the decline")
		}
	})
}

// echoDecline is a derived decline of a charge that keeps its source's echo
// variable: a charge with a new key is answered with that key.
func echoDecline(name string, seq int) *models.Mock {
	return at(onPay(varMock(name, "POST", "/charge", `{"key":"k-rec","amount":5}`, 402, `{"key":"k-rec","declined":true}`,
		models.MockVariables{"key": {Recorded: "k-rec", Bind: "body.key", Answer: []string{"body.key"}, Echo: true}})), seq)
}

// In record's full round the learn step decides a call first: a call that a
// recording that declares variables fits, and that the learn step pairs, as
// drifted, with another recording of the same operation in the test's
// scope, is answered by the learn step from that recording, as before the
// variable pass ran in the full round. A replay answers it by the variable
// pass.
func TestFullRoundLetsTheLearnStepDecideFirst(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, learnOn := range []bool{false, true} {
		round := map[bool]string{true: "record's full round", false: "a replay"}[learnOn]
		charged := at(onPay(varMock("mock-2", "POST", "/charge", `{"key":"k-other","amount":5}`, 200, `{"charged":true}`, nil)), 2)
		db := &varDb{perTest: []*models.Mock{echoDecline("mock-1", 1), charged}}
		got := serveCall(t, openTest(), db, learnOn, "POST", "/charge", `{"key":"k-new","amount":5}`)
		want, taken, left := `{"key":"k-new","declined":true}`, "mock-1", "mock-2"
		if learnOn {
			want, taken, left = `{"charged":true}`, "mock-2", "mock-1"
		}
		if got.body != want || !db.consumed[taken] || db.consumed[left] {
			t.Fatalf("in %s the charge got %d %q (reports %v, taken %v); want %s's answer %s", round, got.status, got.body, got.reported, db.consumed, taken, want)
		}
	}
}

// tookFirstDb is varDb where another connection takes the recording named
// took just as this call first tries to take it: the recording is gone from
// the store, and this call's take fails, as the manager's does.
type tookFirstDb struct {
	*varDb
	took  string
	other map[string]bool // what the other connection took
}

func (d *tookFirstDb) DeleteFilteredMock(m models.Mock) bool {
	if m.Name != d.took || d.other != nil {
		return d.varDb.DeleteFilteredMock(m)
	}
	d.other = map[string]bool{m.Name: true}
	for i, x := range d.perTest {
		if x.Name == m.Name {
			d.perTest = append(d.perTest[:i:i], d.perTest[i+1:]...)
			break
		}
	}
	return false
}

func (d *tookFirstDb) UpdateUnFilteredMock(old, updated *models.Mock) bool {
	return !d.other[old.Name] && d.varDb.UpdateUnFilteredMock(old, updated)
}

// A recording that another connection takes first answers nothing: the call
// is checked again and the next recording that fits answers it. So in record's
// full round, as in a replay, for a recording that declares variables; and,
// in a replay, for the exact recording that alone may answer a call whose
// variables do not hold (in the full round the learn step answers an exact
// call before the variable pass).
func TestARecordingAnotherConnectionTookFirstLeavesTheCallToTheNext(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, learnOn := range []bool{false, true} {
		round := map[bool]string{true: "record's full round", false: "a replay"}[learnOn]
		db := &tookFirstDb{varDb: &varDb{perTest: []*models.Mock{echoDecline("mock-1", 1), echoDecline("mock-2", 2)}}, took: "mock-1"}
		got := serveCall(t, openTest(), db, learnOn, "POST", "/charge", `{"key":"k-new","amount":5}`)
		if got.status != 402 || got.body != `{"key":"k-new","declined":true}` || !db.consumed["mock-2"] {
			t.Fatalf("in %s the charge got %d %q (reports %v, taken %v); want mock-2's decline echoing k-new", round, got.status, got.body, got.reported, db.consumed)
		}
	}
	create := at(onPay(varMock("mock-1", "POST", "/orders", `{"id":"r1"}`, 201, `{"created":true}`,
		models.MockVariables{"orderId": bindV("r1", "body.id")})), 1)
	read := at(onPay(varMock("mock-2", "GET", "/orders/r1", "", 200, `{"state":"paid"}`,
		models.MockVariables{"orderId": useV("r1", []string{"url.path[1]"})})), 2)
	missing := at(onPay(varMock("mock-3", "GET", "/orders/zz", "", 404, `{"missing":1}`, nil)), 3)
	again := at(onPay(varMock("mock-4", "GET", "/orders/zz", "", 404, `{"missing":2}`, nil)), 4)
	db, ctx := &tookFirstDb{varDb: &varDb{perTest: []*models.Mock{create, read, missing, again}}, took: "mock-3"}, openTest()
	if got := serveCall(t, ctx, db, false, "POST", "/orders", `{"id":"l1"}`); got.status != 201 {
		t.Fatalf("the create got %d %q (reports %v); want mock-1's answer", got.status, got.body, got.reported)
	}
	if got := serveCall(t, ctx, db, false, "GET", "/orders/zz", ""); got.status != 404 || got.body != `{"missing":2}` || !db.consumed["mock-4"] {
		t.Fatalf("a read of an order the test never created got %d %q (reports %v, taken %v); want mock-4's answer", got.status, got.body, got.reported, db.consumed)
	}
}

// A call whose context ended is answered by no recording and misses none:
// the full round's variable pass ends with the context's error before it
// reads the store, even where no recording that declares variables is left
// to read, and the variable pass a replay shares ends with it as it checks
// the recordings.
func TestTheVariablePassEndsWithAnEndedContext(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	audit := at(onPay(varMock("mock-1", "POST", "/audit", `{"note":"n"}`, 200, `{"logged":true}`, nil)), 1)
	db := &varDb{perTest: []*models.Mock{audit}}
	stub, diag, decided, err := h.fullRoundVariables(ctx, liveReq("POST", "http://"+fullRoundHost+"/audit", `{"note":"n"}`), db, nil, nil, nil, true)
	if !errors.Is(err, context.Canceled) || !decided || stub != nil || diag != nil {
		t.Fatalf("the full round's pass with its context ended: answer %v miss %+v decided %v err %v; want the context's error", stub, diag, decided, err)
	}
	db = &varDb{perTest: []*models.Mock{echoDecline("mock-1", 1)}}
	vs, err := h.variableStage(ctx, liveReq("POST", "http://"+fullRoundHost+"/charge", `{"key":"k-new","amount":5}`), db, readVarScope(db), db.perTest, nil, nil, nil, true)
	if !errors.Is(err, context.Canceled) || vs.served != nil || vs.diag != nil || vs.retry {
		t.Fatalf("the variable pass with its context ended: %+v err %v; want the context's error", vs, err)
	}
	if db.consumed["mock-1"] {
		t.Fatal("the pass took the decline")
	}
}

// notOwnedDb is varDb under the background rules' ownership
// (integrations.TestOwner) where none of its recordings is the open test's
// own; a recording served and not taken is marked used.
type notOwnedDb struct {
	*varDb
	used map[string]bool
}

func (*notOwnedDb) OwnedByOpenTest(*models.Mock) bool { return false }

func (d *notOwnedDb) MarkMockAsUsed(m models.Mock) bool {
	if d.used == nil {
		d.used = map[string]bool{}
	}
	d.used[m.Name] = true
	return true
}

// A per-test recording of a stateful dependency (its lifetime stated) that is
// not the open test's own is served as a reusable recording is, marked used
// and never taken, in record's full round as in a replay: the variable pass
// reads whose recordings the open test's are from the store.
func TestTheVariablePassTakesOnlyTheOpenTestsOwn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, learnOn := range []bool{false, true} {
		round := map[bool]string{true: "record's full round", false: "a replay"}[learnOn]
		declined := echoDecline("mock-1", 1)
		declined.TestModeInfo.Consume = models.ConsumeCursorSaturate
		declined.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
		db := &notOwnedDb{varDb: &varDb{perTest: []*models.Mock{declined}}}
		got := serveCall(t, openTest(), db, learnOn, "POST", "/charge", `{"key":"k-new","amount":5}`)
		if got.status != 402 || got.body != `{"key":"k-new","declined":true}` {
			t.Fatalf("in %s the charge got %d %q (reports %v); want the decline echoing k-new", round, got.status, got.body, got.reported)
		}
		if db.consumed["mock-1"] || !db.used["mock-1"] || len(db.perTest) != 1 {
			t.Fatalf("in %s the decline, another test's, was taken (taken %v, marked used %v)", round, db.consumed, db.used)
		}
	}
}

// beginsMidPassDb is varDb whose next test begins (its variables' scope
// starts over) while the variable pass checks a call, once armed, just after
// the pass checked the recording before the one named after; the scope ahead
// of the open test (integrations.VariableScopes) holds none of its
// recordings.
type beginsMidPassDb struct {
	*varDb
	ahead variables.Table
	after string
	armed bool
}

func (d *beginsMidPassDb) AheadVariables() *variables.Table { return &d.ahead }

func (d *beginsMidPassDb) InAheadScope(m *models.Mock) bool {
	if d.armed && m.Name == d.after {
		d.armed = false
		d.table.Reset()
	}
	return false
}

// A call whose variables do not hold, checked while a test began, is checked
// again against the scope of the test that began, in record's full round as
// in a replay: its miss is the evidence of that scope (here the variable is
// not yet bound), never of the scope the test that began ended.
func TestACallWhoseVariablesDoNotHoldIsCheckedInTheTestThatBegan(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, learnOn := range []bool{false, true} {
		round := map[bool]string{true: "record's full round", false: "a replay"}[learnOn]
		create := at(onPay(varMock("mock-1", "POST", "/orders", `{"id":"r1"}`, 201, `{"created":true}`,
			models.MockVariables{"orderId": bindV("r1", "body.id")})), 1)
		read := at(onPay(varMock("mock-2", "GET", "/orders/r1", "", 200, `{"state":"paid"}`,
			models.MockVariables{"orderId": useV("r1", []string{"url.path[1]"})})), 2)
		db, ctx := &beginsMidPassDb{varDb: &varDb{perTest: []*models.Mock{create, read, echoDecline("mock-3", 3)}}, after: "mock-3"}, openTest()
		if got := serveCall(t, ctx, db, learnOn, "POST", "/orders", `{"id":"l1"}`); got.status != 201 {
			t.Fatalf("in %s the create got %d %q (reports %v); want mock-1's answer", round, got.status, got.body, got.reported)
		}
		db.armed = true
		got := serveCall(t, ctx, db, learnOn, "GET", "/orders/zz", "")
		if got.status != http.StatusBadGateway {
			t.Fatalf("in %s the read got %d %q; want a miss", round, got.status, got.body)
		}
		if r := reportOf(t, got); r.MatchPhase != models.MatchPhaseVariable || r.ClosestMock != "mock-2" || !strings.Contains(r.Diff, "before the call that binds it") {
			t.Fatalf("in %s the read's miss is %+v; want mock-2's, its variable not yet bound in the test that began", round, r)
		}
		if db.armed {
			t.Fatalf("in %s no test began while the read was checked", round)
		}
	}
}

// A series of reusable (ConsumeCursorSaturate) recordings of one call that
// declare variables (two charges, each with a fresh key the dependency
// echoes) answers in record order in record's full round as in a replay:
// each once, and the call after the series stays on its last, as a
// saturating cursor does. The full round's variable pass walks the series
// as a replay's does, stateful recordings on.
func TestFullRoundAnswersASeriesOfRecordingsWithVariablesInRecordOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	echo := func(name, key string, seq int) *models.Mock {
		return reusable(at(onPay(varMock(name, "POST", "/charge", `{"key":"`+key+`"}`, 200, `{"echo":"`+key+`","n":"`+name+`"}`,
			models.MockVariables{"key": {Recorded: key, Bind: "body.key", Answer: []string{"body.echo"}, Echo: true}})), seq))
	}
	want := []string{`{"echo":"x-1","n":"mock-a"}`, `{"echo":"x-2","n":"mock-b"}`, `{"echo":"x-3","n":"mock-b"}`}
	for _, learnOn := range []bool{false, true} {
		round := map[bool]string{true: "record's full round", false: "a replay"}[learnOn]
		db, ctx := &varDb{session: []*models.Mock{echo("mock-b", "k-2", 2), echo("mock-a", "k-1", 1)}}, openTest()
		var got []string
		for i, live := range []string{"x-1", "x-2", "x-3"} {
			g := serveCall(t, ctx, db, learnOn, "POST", "/charge", `{"key":"`+live+`"}`)
			if g.status != 200 {
				t.Fatalf("in %s charge %d got %d %q (reports %v); want the series' answer", round, i+1, g.status, g.body, g.reported)
			}
			got = append(got, g.body)
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("in %s the charges were answered %v; want %v", round, got, want)
		}
	}
}

// beginsInReadDb is varDb whose next test begins, once armed, inside its
// read of the per-test recordings: the read gives the ending test's
// recordings, then the next test's are swapped in and its variables' scope
// starts over, as a test begins (its pool swapped in, then its scope
// begun).
type beginsInReadDb struct {
	*varDb
	next  []*models.Mock
	armed bool
}

func (d *beginsInReadDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	got, err := d.varDb.GetPerTestMocksInWindow()
	if d.armed {
		d.armed = false
		d.perTest = d.next
		d.table.Reset()
	}
	return got, err
}

// A call checked while a test begins during the read of its pool is
// answered in the test that began, its pool and its variables both: record's
// full round reads the variables' scope before the pool, so the binds of a
// call checked against the ending test's recordings are stale in the test
// that began (variables.Table.BindAll), and the call is checked again. Here
// the ending test's create binds oldId, the next test's binds newId: the
// create is answered by the next test's recording, and oldId is never bound
// in the test that began.
func TestFullRoundAnswersACallInTheTestThatBeganWhileItsPoolWasRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ending := at(onPay(varMock("mock-1", "POST", "/orders", `{"id":"r1"}`, 201, `{"test":"ending"}`,
		models.MockVariables{"oldId": bindV("r1", "body.id")})), 1)
	began := at(onPay(varMock("mock-9", "POST", "/orders", `{"id":"r9"}`, 201, `{"test":"began"}`,
		models.MockVariables{"newId": bindV("r9", "body.id")})), 9)
	db := &beginsInReadDb{varDb: &varDb{perTest: []*models.Mock{ending}}, next: []*models.Mock{began}, armed: true}
	stub, diag, decided, err := newHTTP().fullRoundVariables(openTest(), liveReq("POST", "http://"+fullRoundHost+"/orders", `{"id":"l1"}`), db, nil, nil, nil, true)
	if err != nil || !decided || stub == nil || stub.Spec.HTTPResp.Body != `{"test":"began"}` {
		t.Fatalf("the create got answer %v, miss %+v, decided %v, err %v; want mock-9's, the test that began", stub, diag, decided, err)
	}
	if db.armed {
		t.Fatal("no test began while the create's pool was read")
	}
	if b, ok := db.table.Lookup("oldId"); ok {
		t.Fatalf("the ending test's create bound %s=%q in the test that began", b.Name, b.Value)
	}
	if b, ok := db.table.Lookup("newId"); !ok || b.Value != "l1" || !db.consumed["mock-9"] || db.consumed["mock-1"] {
		t.Fatalf("newId is %+v (bound %v), taken %v; want newId=l1 bound by mock-9, mock-9 taken", b, ok, db.consumed)
	}
}

// A recording that declares variables is compared with the call under the
// request noise a replay compares it under, in record's full round as in a
// replay: the flaky headers keploy excuses by itself (a traceparent, new on
// every request), and keploy.yml's request body and URL noise. A charge
// with a new key that differs from the recording only there is answered,
// echoing its key, in both rounds; without that noise it misses in both.
func TestFullRoundComparesARecordingWithVariablesUnderTheReplaysNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	charge := func(path, body string, header map[string]string) *varDb {
		m := at(onPay(varMock("mock-1", "POST", path, body, 200, `{"echo":"k-1"}`,
			models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{"body.echo"}, Echo: true}})), 1)
		for k, v := range header {
			m.Spec.HTTPReq.Header[k] = v
		}
		return &varDb{perTest: []*models.Mock{m}}
	}
	for _, c := range []struct {
		name          string
		noised, plain models.OutgoingOptions
		recorded      func() *varDb
		path, body    string
		headers       []string
	}{{
		name:   "a traceparent, a flaky header",
		noised: models.OutgoingOptions{},
		plain:  models.OutgoingOptions{DisableAutoHeaderNoise: true},
		recorded: func() *varDb {
			return charge("/charge", `{"key":"k-1"}`, map[string]string{"Traceparent": "00-11111111111111111111111111111111-2222222222222222-01"})
		},
		path:    "/charge",
		body:    `{"key":"x-1"}`,
		headers: []string{"Traceparent: 00-33333333333333333333333333333333-4444444444444444-01"},
	}, {
		name:     "keploy.yml's request body noise",
		noised:   models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"requestbody": {"nonce": {}}}},
		recorded: func() *varDb { return charge("/charge", `{"key":"k-1","nonce":"n-1"}`, nil) },
		path:     "/charge",
		body:     `{"key":"x-1","nonce":"n-2"}`,
	}, {
		name:     "keploy.yml's URL noise",
		noised:   models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": {"/charge/day-[a-z]+": {}}}},
		recorded: func() *varDb { return charge("/charge/day-mon", `{"key":"k-1"}`, nil) },
		path:     "/charge/day-tue",
		body:     `{"key":"x-1"}`,
	}} {
		for _, learnOn := range []bool{false, true} {
			round := map[bool]string{true: "record's full round", false: "a replay"}[learnOn]
			opts := c.noised
			opts.Learn = learnOn
			got := serveCallWith(t, openTest(), c.recorded(), opts, "POST", c.path, c.body, c.headers...)
			if got.status != 200 || got.body != `{"echo":"x-1"}` {
				t.Fatalf("%s: in %s the charge got %d %q (reports %v); want mock-1's answer echoing x-1", c.name, round, got.status, got.body, got.reported)
			}
			opts = c.plain
			opts.Learn = learnOn
			if got := serveCallWith(t, openTest(), c.recorded(), opts, "POST", c.path, c.body, c.headers...); got.status == 200 {
				t.Fatalf("%s: in %s without that noise the charge got %d %q; want a miss", c.name, round, got.status, got.body)
			}
		}
	}
}
