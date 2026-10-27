package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// s12TestDb is the store as an open test of a set served by the background
// rules sees it: its own cursor keeps none (a test's own recordings answer once
// each, in recorded order), and its background's cursor (BackgroundCursor) is
// the one the calls between tests advance.
type s12TestDb struct {
	mockMemDb
	bg *mockMemDb
}

func (d *s12TestDb) MockCursorIndex(string, int) int    { return 0 }
func (d *s12TestDb) AdvanceMockCursor(string, int, int) {}
func (d *s12TestDb) BackgroundCursor() (integrations.MockCursor, bool) {
	return d.bg, true
}

var _ integrations.BackgroundCursor = (*s12TestDb)(nil)

// statefulAt is a stateful recording (ConsumeCursorSaturate) of one call,
// recorded at at.
func statefulAt(name string, at time.Time) *models.Mock {
	m := onHost(name, "/status", "api.example")
	m.TestModeInfo.Consume = models.ConsumeCursorSaturate
	m.Spec.ReqTimestampMock = at
	return m
}

// Under the background rules the open test's own recordings and the set's
// background are two bands, never one sequence: a test's call
// that its own recording is is answered by it, though background recordings of
// the call were recorded before it; a call only background recordings are
// advances through them on the background's cursor, the one the calls between
// tests advance, so a poller sees its recorded sequence wherever its calls
// land, then the last again. The variable pass picks the same way. Under
// today's rules (no background cursor) one sequence in record order, as before.
func TestS12StatefulPickKeepsTheTestsOwnApartFromTheBackground(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Unix(1_700_000_000, 0)
	starting := statefulAt("bg-starting", base)
	ready := statefulAt("bg-ready", base.Add(time.Second))
	own := statefulAt("own", base.Add(2*time.Second))
	isOwn := func(m *models.Mock) bool { return m == own }
	h := newHTTP()
	db := &s12TestDb{bg: &mockMemDb{}}
	pick := func(fits ...*models.Mock) string {
		m, commit, _ := h.strictCursorPick(fits, isOwn, db, true, nil)
		if commit != nil {
			commit()
		}
		return m.Name
	}
	// The pool's order puts the test's own first.
	if got := pick(own, starting, ready); got != "own" {
		t.Fatalf("the test's own call was answered by %s; want its own recording", got)
	}
	for i, want := range []string{"bg-starting", "bg-ready", "bg-ready"} {
		if got := pick(ready, starting); got != want {
			t.Fatalf("background pick %d = %s; want %s (the background's sequence, then the last again)", i+1, got, want)
		}
	}
	if len(db.bg.cursors) == 0 {
		t.Fatal("the background's picks kept no mark in the background's cursor")
	}

	// The variable pass.
	hits := []*varHit{{mock: own}, {mock: starting}, {mock: ready}}
	if hit, _, _ := h.pickHit(hits, isOwn, &s12TestDb{bg: &mockMemDb{}}, true, nil); hit.mock != own {
		t.Fatalf("the variable pass answered the test's own call with %s", hit.mock.Name)
	}
	vdb := &s12TestDb{bg: &mockMemDb{}}
	for i, want := range []string{"bg-starting", "bg-ready", "bg-ready"} {
		hit, commit, _ := h.pickHit([]*varHit{{mock: ready}, {mock: starting}}, isOwn, vdb, true, nil)
		if commit != nil {
			commit()
		}
		if hit.mock.Name != want {
			t.Fatalf("variable pass background pick %d = %s; want %s", i+1, hit.mock.Name, want)
		}
	}

	// Today's rules: one sequence across the tiers, in record order.
	plain := &mockMemDb{}
	if m, _, _ := h.strictCursorPick([]*models.Mock{own, starting, ready}, isOwn, plain, true, nil); m != starting {
		t.Fatalf("today's rules picked %s; want bg-starting, the first recorded, as before", m.Name)
	}
	if hit, _, _ := h.pickHit([]*varHit{{mock: own}, {mock: starting}}, isOwn, plain, true, nil); hit.mock != starting {
		t.Fatalf("today's variable pass picked %s; want bg-starting, as before", hit.mock.Name)
	}
}

// s12LearnDb is learnDb serving its set by the background rules
// (integrations.S12Background).
type s12LearnDb struct {
	*learnDb
	bg mockMemDb
}

func (d *s12LearnDb) BackgroundCursor() (integrations.MockCursor, bool) { return &d.bg, true }

// Under the background rules a test learns from its own recordings only
// (own exact, own learn, then background exact): with a test
// open the background answers a call exactly or not at all, and nothing is
// learned onto it. A call in a test that a pool recording is closer to (fewer
// drifted leaves) than the test's own is then a miss, as learn.Pair makes any
// call whose closest recording it may not learn onto: never learned onto the
// pool recording, nor paired with the farther own one. The pool recording
// still answers a call that is it exactly, and between tests the pool is
// learned from. Under today's rules the closer pool recording is paired, as
// before.
func TestS12ATestLearnsOnlyFromItsOwn(t *testing.T) {
	for _, s12 := range []bool{true, false} {
		h, n, opts := learnOn(t)
		opts.LearnPool = true
		own := learnRecorded("own", 0, "POST", "/a", `{"id":"x","n":1,"m":424242}`, nil, `{}`)
		pool := learnRecorded("pool", -time.Hour, "POST", "/a", `{"id":"x","n":1,"m":2}`, nil, `{}`)
		pool.TestModeInfo.Lifetime = models.LifetimeSession
		ld := &learnDb{perTest: []*models.Mock{own}, mockMemDb: mockMemDb{mocks: []*models.Mock{pool}}}
		var db integrations.MockMemDb = ld
		if s12 {
			db = &s12LearnDb{learnDb: ld}
		}
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		served, miss, err := h.learnMatch(ctx, live("POST", "/a", `{"id":"x","n":9,"m":2}`, nil), db, n, opts)
		if err != nil {
			t.Fatal(err)
		}
		if !s12 {
			if served == nil || served.Name != "pool" || len(ld.pooled) != 1 {
				t.Fatalf("today's rules: served %v, %d learned onto the pool; want the closer pool recording paired, as before", served, len(ld.pooled))
			}
			continue
		}
		if served != nil || miss == nil || miss.closest != pool || miss.res.Reason != models.MissRequestChanged {
			t.Fatalf("the background rules served %v (miss %+v); want a miss naming the pool recording", served, miss)
		}
		if len(ld.pooled) != 0 || len(ld.consumed) != 0 {
			t.Fatalf("the background rules learned onto the pool %d times and consumed %d of the test's own", len(ld.pooled), len(ld.consumed))
		}
		// The pool recording still answers a call that is it exactly.
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/a", `{"id":"x","n":1,"m":2}`, nil), db, n, opts); served == nil || served.Name != "pool" || miss != nil {
			t.Fatalf("the pool recording did not answer its own call exactly in a test: %v %+v", served, miss)
		}
		// The test's own call learns from its own.
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/a", `{"id":"x","n":1,"m":31415}`, nil), db, n, opts); served == nil || served.Name != "own" || miss != nil {
			t.Fatalf("the test's own call was not learned from its own: %v %+v", served, miss)
		}
		// Between tests the pool is learned from.
		gap := learn.WithTracker(context.Background(), learn.NewTracker())
		if served, miss, _ := h.learnMatch(gap, live("POST", "/a", `{"id":"x","n":7,"m":2}`, nil), db, n, opts); served == nil || served.Name != "pool" || miss != nil {
			t.Fatalf("between tests the pool was not learned from: %v %+v", served, miss)
		}
	}
}

// roundLearnDb is s12LearnDb read in one snapshot of the round
// (integrations.RoundReader), as the agent's MockManager is under exact
// serving: the store shows test's view ("" the pool view), the tracker the
// query names is read with it, and background says the set is served by
// the background rules. The view holds its per-test tier as test's own;
// its session tier is the pool's (the replay's scope table). A consume
// through the round lands in s12LearnDb.
type roundLearnDb struct {
	*s12LearnDb
	test       string
	background bool
}

func (d *roundLearnDb) RoundSnapshot(q integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	s := integrations.RoundSnapshot{Test: d.test, Background: d.background, PerTest: d.perTest, Session: d.mocks}
	if t := q.Tracker; t != nil {
		p := t.Placed()
		s.Learn = integrations.LearnRead{Bound: true, Number: p.Test, Open: p.Open, Begun: p.Begun}
		s.Learn.Earlier, s.Learn.Served, s.Learn.Before, s.Learn.Awaiting = p.Earlier, p.Served, p.Before, p.Awaiting
		s.Learn.Answered = t.AnsweredMocks()
	}
	switch {
	case d.test != "" && s.Learn.Open:
		s.Phase = integrations.RoundInTest
	case d.test == "" && !s.Learn.Open:
		s.Phase = integrations.RoundBetween
	default:
		s.Phase = integrations.RoundBoundary
	}
	s.Open = s.Phase == integrations.RoundInTest
	owners := map[string]integrations.RoundOwner{}
	if d.test != "" {
		for _, m := range d.perTest {
			owners[m.Name] = integrations.OwnerTest
		}
	}
	pool := map[string]struct{}{}
	for _, m := range d.mocks {
		pool[m.Name] = struct{}{}
	}
	return s.WithParts(integrations.RoundParts{Owners: owners, PoolNames: &pool}), true
}

func (d *roundLearnDb) WithinRound(integrations.RoundAt) integrations.RoundScope {
	return roundLearnScope{d.s12LearnDb}
}

func (d *roundLearnDb) ClaimPairingIn(*models.Mock, integrations.RoundAt) (claimed, moved bool, why string) {
	return false, false, "no pairing in the learn step"
}

func (d *roundLearnDb) SpendByKindIn(*models.Mock, integrations.RoundAt) (spent, moved bool, why string) {
	return false, false, "no background spend in the learn step"
}

func (d *roundLearnDb) TakenLog(uint64, int, int) []integrations.Take { return nil }

// roundLearnScope is s12LearnDb as a RoundScope whose round never moves.
type roundLearnScope struct{ *s12LearnDb }

func (roundLearnScope) Moved() string { return "" }

var _ integrations.RoundReader = (*roundLearnDb)(nil)

// Read from one snapshot of the round (the agent's production path), the
// background answers a call exactly or not at all, and nothing is learned
// onto it, wherever the store shows a test's view: with the tracker's test
// open in it (in a test), and with the tracker having none open (a boundary:
// the view opened before the tracker began its test). Only where the store
// shows the pool view and no test is open is the pool learned from. Under
// today's rules the closer pool recording is paired in a test, as before.
func TestS12ATestLearnsOnlyFromItsOwnInOneReadOfTheRound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		test       string
		begun      bool
		background bool
		learned    bool
	}{
		{"in a test", "test-1", true, true, false},
		{"a test's view the tracker has not begun", "test-1", false, true, false},
		{"between tests", "", false, true, true},
		{"in a test, under today's rules", "test-1", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			opts.LearnPool = true
			own := learnRecorded("own", 0, "POST", "/a", `{"id":"x","n":1,"m":424242}`, nil, `{}`)
			pool := learnRecorded("pool", -time.Hour, "POST", "/a", `{"id":"x","n":1,"m":2}`, nil, `{}`)
			pool.TestModeInfo.Lifetime = models.LifetimeSession
			ld := &learnDb{perTest: []*models.Mock{own}, mockMemDb: mockMemDb{mocks: []*models.Mock{pool}}}
			db := &roundLearnDb{s12LearnDb: &s12LearnDb{learnDb: ld}, test: tc.test, background: tc.background}
			tr := learn.NewTracker()
			if tc.begun {
				tr.Begin()
			}
			ctx := learn.WithTracker(context.Background(), tr)
			served, miss, err := h.learnMatch(ctx, live("POST", "/a", `{"id":"x","n":9,"m":2}`, nil), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.learned {
				if served == nil || served.Name != "pool" || miss != nil || len(ld.pooled) != 1 {
					t.Fatalf("served %v (miss %+v), %d learned onto the pool; want the pool recording learned from", served, miss, len(ld.pooled))
				}
				return
			}
			if served != nil || miss == nil || miss.closest != pool || miss.res.Reason != models.MissRequestChanged {
				t.Fatalf("served %v (miss %+v); want a miss naming the pool recording", served, miss)
			}
			if len(ld.pooled) != 0 || len(ld.consumed) != 0 {
				t.Fatalf("learned onto the pool %d times and consumed %d of the test's own", len(ld.pooled), len(ld.consumed))
			}
			// The pool recording still answers a call that is it exactly.
			if served, miss, _ := h.learnMatch(ctx, live("POST", "/a", `{"id":"x","n":1,"m":2}`, nil), db, n, opts); served == nil || served.Name != "pool" || miss != nil {
				t.Fatalf("the pool recording did not answer its own call exactly: %v %+v", served, miss)
			}
			if !tc.begun {
				return
			}
			// The test's own call learns from its own.
			if served, miss, _ := h.learnMatch(ctx, live("POST", "/a", `{"id":"x","n":1,"m":31415}`, nil), db, n, opts); served == nil || served.Name != "own" || miss != nil || len(ld.consumed) != 1 {
				t.Fatalf("the test's own call was not learned from its own: %v %+v", served, miss)
			}
		})
	}
}

// s12ExactDb is s12TestDb under exact serving: the open test's view
// (SequenceViewer), whose cursor is the test's own, and its recorded window.
// staged is every recording the store stages (integrations.StagedReader),
// spent ones included.
type s12ExactDb struct {
	s12TestDb
	view   integrations.SequenceView
	staged []*models.Mock
}

func (d *s12ExactDb) SequenceView() (integrations.SequenceView, bool) { return d.view, true }
func (d *s12ExactDb) StagedMocks() []*models.Mock                     { return d.staged }

// Under exact serving too, a background call walks the background's
// sequence on the background's cursor, whichever pass picks it (cursorPick,
// the learn step's and the replay of learned noise's pick; the strict pass;
// the variable pass): from its first recording, then the last again,
// wherever its calls land. The test's own cursor and recorded window never restart it, which
// would answer each test the recording in force when it began, however far
// the background's sequence had gone.
func TestS12TheBackgroundWalksOnItsCursorUnderExactServing(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Unix(1_700_000_000, 0)
	starting := statefulAt("bg-starting", base)
	ready := statefulAt("bg-ready", base.Add(time.Second))
	own := statefulAt("own", base.Add(2*time.Second))
	isOwn := func(m *models.Mock) bool { return m == own }
	h := newHTTP()
	var bg *mockMemDb
	test := func(name string) *s12ExactDb {
		return &s12ExactDb{s12TestDb: s12TestDb{bg: bg}, view: integrations.SequenceView{
			Cursor: &mockMemDb{}, Test: name, Start: base.Add(1500 * time.Millisecond), End: base.Add(3 * time.Second)}}
	}
	for _, p := range []struct {
		name string
		pick func(db *s12ExactDb) (*models.Mock, func(), *answeredSequence)
	}{
		{"cursorPick", func(db *s12ExactDb) (*models.Mock, func(), *answeredSequence) {
			return h.cursorPick(ready, []*models.Mock{ready, starting}, isOwn, db, true)
		}},
		{"the strict pass", func(db *s12ExactDb) (*models.Mock, func(), *answeredSequence) {
			return h.strictCursorPick([]*models.Mock{ready, starting}, isOwn, db, true, nil)
		}},
		{"the variable pass", func(db *s12ExactDb) (*models.Mock, func(), *answeredSequence) {
			hit, commit, answered := h.pickHit([]*varHit{{mock: ready}, {mock: starting}}, isOwn, db, true, nil)
			if hit == nil {
				return nil, commit, answered
			}
			return hit.mock, commit, answered
		}},
	} {
		bg = &mockMemDb{}
		a, b := test("A"), test("B")
		for i, c := range []struct {
			db   *s12ExactDb
			want string
		}{{a, "bg-starting"}, {a, "bg-ready"}, {b, "bg-ready"}} {
			m, commit, answered := p.pick(c.db)
			if answered != nil {
				t.Fatalf("%s: background pick %d in test %s missed: %s", p.name, i+1, c.db.view.Test, answered.why())
			}
			if m == nil || m.Name != c.want {
				t.Fatalf("%s: background pick %d in test %s = %v; want %s (the background's sequence on its cursor, then the last again)", p.name, i+1, c.db.view.Test, m, c.want)
			}
			if commit != nil {
				commit()
			}
		}
		if len(bg.cursors) == 0 {
			t.Fatalf("%s: the background's picks kept no mark in the background's cursor", p.name)
		}
	}
	a := test("A")
	if m, _, _ := h.cursorPick(own, []*models.Mock{own, starting, ready}, isOwn, a, true); m != own {
		t.Fatalf("the test's own call was answered by %v; want its own recording", m)
	}
}

// Under exact serving the background's sequence is fixed by every recording
// of it the store stages, as a test's is: once its last recording can answer
// no more (a worker scope's answers once in the staging), a call the
// background answers is a miss that names it, in every pass (cursorPick, the
// strict pass, the variable pass), whichever test is open; never one of its
// earlier recordings again, which would take a poller's sequence back (a job
// read as running after it was read as done). The last may have answered
// before it was spent, or another call may have spent it first.
func TestS12TheBackgroundsSpentLastIsAMissNotAnEarlierOneAgain(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Unix(1_700_000_000, 0)
	starting := statefulAt("bg-starting", base)
	ready := statefulAt("bg-ready", base.Add(time.Second))
	done := statefulAt("bg-done", base.Add(1200*time.Millisecond))
	own := statefulAt("own", base.Add(2*time.Second))
	isOwn := func(m *models.Mock) bool { return m == own }
	every := func(*models.Mock) bool { return true }
	h := newHTTP()
	for _, p := range []struct {
		name string
		pick func(db *s12ExactDb, live []*models.Mock) (*models.Mock, func(), *answeredSequence)
	}{
		{"cursorPick", func(db *s12ExactDb, live []*models.Mock) (*models.Mock, func(), *answeredSequence) {
			return h.cursorPick(live[0], live, isOwn, db, true)
		}},
		{"the strict pass", func(db *s12ExactDb, live []*models.Mock) (*models.Mock, func(), *answeredSequence) {
			return h.strictCursorPick(live, isOwn, db, true, every)
		}},
		{"the variable pass", func(db *s12ExactDb, live []*models.Mock) (*models.Mock, func(), *answeredSequence) {
			hits := make([]*varHit, 0, len(live))
			for _, m := range live {
				hits = append(hits, &varHit{mock: m})
			}
			hit, commit, answered := h.pickHit(hits, isOwn, db, true, every)
			if hit == nil {
				return nil, commit, answered
			}
			return hit.mock, commit, answered
		}},
	} {
		for _, c := range []struct {
			name    string
			answers []string // the background's calls while bg-done is live
		}{
			{"the last answered, then was spent", []string{"bg-starting", "bg-ready", "bg-done"}},
			{"another call spent the last first", []string{"bg-starting", "bg-ready"}},
		} {
			bg := &mockMemDb{}
			db := func(test string) *s12ExactDb {
				return &s12ExactDb{s12TestDb: s12TestDb{bg: bg}, staged: []*models.Mock{starting, ready, done, own},
					view: integrations.SequenceView{Cursor: &mockMemDb{}, Test: test,
						Start: base.Add(1500 * time.Millisecond), End: base.Add(3 * time.Second)}}
			}
			for i, want := range c.answers {
				m, commit, answered := p.pick(db("A"), []*models.Mock{ready, starting, done})
				if answered != nil {
					t.Fatalf("%s, %s: background call %d missed: %s", p.name, c.name, i+1, answered.why())
				}
				if m == nil || m.Name != want {
					t.Fatalf("%s, %s: background call %d = %v; want %s", p.name, c.name, i+1, m, want)
				}
				if commit != nil {
					commit()
				}
			}
			// bg-done is spent: the store no longer offers it, but stages it.
			m, _, answered := p.pick(db("B"), []*models.Mock{ready, starting})
			if answered == nil {
				t.Fatalf("%s, %s: once bg-done is spent the background call was answered %s; want a miss, never an earlier recording again", p.name, c.name, m.Name)
			}
			if why := answered.why(); !strings.Contains(why, "bg-done") || !strings.Contains(why, "3 recordings") {
				t.Fatalf("%s, %s: the miss's reason %q does not name bg-done, the last of the background's 3 recordings", p.name, c.name, why)
			}
		}
	}
}
