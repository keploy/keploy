package integrations

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// A call's mark is moved, at a later read of the round, by another staging
// or another boundary count of the round, and by nothing else: not by more
// takes, nor by a generation that moved alone (a test process's claim of its
// key again). A mark whose store keeps no generation is moved at every read
// (nothing tells the round did not move under it: fail closed); the zero
// CallRound, of no read, is never moved, its later read being the call's
// first. A round snapshot's call round carries its own read's instant, so it
// is not moved by it.
func TestACallsMarkIsMovedByAnotherStagingOrBoundaryOfTheRound(t *testing.T) {
	at := RoundAt{Epoch: 3, Gen: 7, Bounds: 5, Taken: 2}
	marked := NewCallRound(3, nil).ReadAt(at)
	for _, tc := range []struct {
		name  string
		call  CallRound
		later RoundAt
		moved bool
	}{
		{"the same round", marked, at, false},
		{"the same round, more taken since", marked, RoundAt{Epoch: 3, Gen: 7, Bounds: 5, Taken: 9}, false},
		{"another generation alone (a key claimed again)", marked, RoundAt{Epoch: 3, Gen: 8, Bounds: 5, Taken: 2}, false},
		{"another boundary count", marked, RoundAt{Epoch: 3, Gen: 8, Bounds: 6, Taken: 2}, true},
		{"another staging", marked, RoundAt{Epoch: 4, Gen: 7, Bounds: 5}, true},
		{"a mark that keeps no generation", NewCallRound(3, nil), at, true},
		{"a mark that keeps no generation, read at zero", NewCallRound(3, nil), RoundAt{Epoch: 3}, true},
		{"the zero CallRound", CallRound{}, at, false},
		{"a mark at zero, read again there", NewCallRound(3, nil).ReadAt(RoundAt{Epoch: 3}), RoundAt{Epoch: 3}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.call.MovedSince(tc.later); got != tc.moved {
				t.Fatalf("MovedSince(%+v) = %v; want %v", tc.later, got, tc.moved)
			}
		})
	}
	want := RoundAt{Epoch: 3, Gen: 7, Bounds: 5}
	if got, known := NewCallRound(1, nil).ReadAt(at).InStaging(true).InView(true, true).Round(); !known || got != want {
		t.Fatalf("a mark's round is lost across its other settings: %+v (known %v); want %+v", got, known, want)
	}
	if _, known := NewCallRound(3, nil).Round(); known {
		t.Fatal("a mark of no generation says it knows its round")
	}
	for name, s := range map[string]RoundSnapshot{
		"a test's view":                  {At: at, Phase: RoundInTest, Test: "T1", Open: true},
		"the tracker ahead":              {At: at, Phase: RoundBoundary, Learn: LearnRead{Bound: true, Open: true}},
		"a set the store has not staged": {At: at, Phase: RoundBoundary, Learn: LearnRead{Bound: true, SetAhead: true}},
	} {
		if name == "the tracker ahead" && !s.TrackerAhead() || name == "a set the store has not staged" && !s.SetUnstaged() {
			t.Fatalf("precondition: %s does not read so", name)
		}
		c := s.CallRound()
		if got, known := c.Round(); !known || got != want || c.MovedSince(at) {
			t.Errorf("%s: a round snapshot's call round reads %+v (known %v), moved by its own read %v; want %+v", name, got, known, c.MovedSince(at), want)
		}
	}
}

// A view's sequence of a reusable request walks, in a test with a recorded
// window, the recordings the window holds; with none in it, the one in force
// when the test began (the last before it), else the first; between tests,
// and in a test with no window, every one.
func TestASequenceWalksTheOpenTestsRecordedWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := func(secs ...int) func(int) time.Time {
		return func(i int) time.Time { return t0.Add(time.Duration(secs[i]) * time.Second) }
	}
	in := func(from, to int) SequenceView {
		return SequenceView{Test: "T", Start: t0.Add(time.Duration(from) * time.Second), End: t0.Add(time.Duration(to) * time.Second)}
	}
	for _, c := range []struct {
		name   string
		v      SequenceView
		secs   []int
		lo, hi int
	}{
		{"between tests", SequenceView{}, []int{1, 2, 3}, 0, 3},
		{"a test with no window", SequenceView{Test: "T"}, []int{1, 2, 3}, 0, 3},
		{"the window's own", in(2, 3), []int{1, 2, 3, 4}, 1, 3},
		{"bounds included", in(1, 4), []int{1, 2, 3, 4}, 0, 4},
		{"none in it: the last before", in(5, 6), []int{1, 2, 7}, 1, 2},
		{"none before or in it: the first", in(0, 0), []int{1, 2}, 0, 1},
		{"none recorded", in(1, 2), nil, 0, 0},
	} {
		lo, hi := c.v.Span(len(c.secs), at(c.secs...))
		if lo != c.lo || hi != c.hi {
			t.Errorf("%s: span [%d, %d); want [%d, %d)", c.name, lo, hi, c.lo, c.hi)
		}
	}
}

// A miss of a marked series (SeriesMiss.Mark) is the set's background miss,
// named as the series' newest recording, saying why the series answered
// none, and showing nothing of the closest recording the parser's report
// read in the open view: its request, which the replay prints against the
// call under the name the report shows, its diff, field diffs, the fields
// that differ and their reasons are of a recording that is no recording of
// the series. What else the report says of the call stays.
func TestASeriesMissShowsNothingOfTheOpenViewsClosest(t *testing.T) {
	r := &models.MockMismatchReport{
		Protocol: "HTTP", ActualSummary: "GET /internal/claims/next", ReceivedReq: "GET /internal/claims/next?since=1793000470",
		ClosestMock: "mock-own", ClosestMockReq: "GET /internal/claims/next?since=1793000410", Diff: "url.query.since differs",
		FieldDiffs:   []models.MockFieldDiff{{Path: "url.query.since"}},
		Differs:      []string{"url.query.since"},
		FieldReasons: []models.FieldReason{{Field: "url.query.since", Why: "url.query.since was learned as a read of the application's clock"}},
		NextSteps:    "the open view's closest's next steps",
	}
	(&SeriesMiss{NextSteps: "its reads of the clock do not hold on this run's clock", Newest: &models.Mock{Name: "mock-poll-12"}}).Mark(r)
	if !r.Background || r.ClosestMock != "mock-poll-12" || r.NextSteps != "its reads of the clock do not hold on this run's clock" {
		t.Fatalf("the series' miss is background %v, closest %q, next steps %q; want the set's background miss naming mock-poll-12 with the series' why", r.Background, r.ClosestMock, r.NextSteps)
	}
	if r.ClosestMockReq != "" || r.Diff != "" || r.FieldDiffs != nil || r.Differs != nil || r.FieldReasons != nil {
		t.Fatalf("the series' miss names mock-poll-12 but shows mock-own's request %q, diff %q, fields %v %v %v", r.ClosestMockReq, r.Diff, r.FieldDiffs, r.Differs, r.FieldReasons)
	}
	if r.Protocol != "HTTP" || r.ActualSummary != "GET /internal/claims/next" || r.ReceivedReq != "GET /internal/claims/next?since=1793000470" {
		t.Fatalf("the series' miss lost what it says of the call: %+v", r)
	}
}

// A miss of a test's own series (SeriesMiss.Owner) is that test's own miss:
// a call of its own job that nothing answers in its own window is never
// passed off as the set's background work.
func TestAnOwnSeriesMissIsItsOwnersNotTheBackgrounds(t *testing.T) {
	r := &models.MockMismatchReport{Protocol: "Mongo", ClosestMock: "mock-own"}
	(&SeriesMiss{NextSteps: "its reads of the clock do not hold on this run's clock", Newest: &models.Mock{Name: "mock-beat-8"}, Owner: "T1"}).Mark(r)
	if r.Background || r.ClosestMock != "mock-beat-8" || r.NextSteps != "its reads of the clock do not hold on this run's clock" {
		t.Fatalf("the own series' miss is background %v, closest %q, next steps %q; want T1's own miss naming mock-beat-8", r.Background, r.ClosestMock, r.NextSteps)
	}
}

// OwnedBy hands a call made in a test's window the series marked set-wide
// and that test's own series, never another test's own series; between
// tests ("") it hands no test's own series.
func TestOwnedByLeavesOutOtherTestsOwnSeries(t *testing.T) {
	all := []MarkedSeries{{Owner: ""}, {Owner: "T1"}, {Owner: "T2"}}
	owners := func(s []MarkedSeries) string {
		out := []string{}
		for _, m := range s {
			out = append(out, "<"+m.Owner+">")
		}
		return strings.Join(out, ",")
	}
	for test, want := range map[string]string{"T1": "<>,<T1>", "T2": "<>,<T2>", "": "<>", "T3": "<>"} {
		if got := owners(OwnedBy(all, test)); got != want {
			t.Fatalf("OwnedBy(%q) = %s; want %s", test, got, want)
		}
	}
	if got := OwnedBy(all[:1], "T1"); &got[0] != &all[0] {
		t.Fatalf("OwnedBy copied a list it leaves nothing out of")
	}
}

// A test's own series is compared on the ids its window bound this run: the
// job's beat on this run's id is the series' call, given back the recorded
// id; a beat on the recorded id, on a third id, or on an id two variables
// bound to two values is the owner's miss, said why; a series marked
// set-wide, and an id no variable was recorded as, are compared as recorded.
func TestOwnCallGivesBackTheIDsTheOwnersWindowBound(t *testing.T) {
	leaf := func(p, v string) learn.Leaf { return learn.Leaf{Path: p, Type: learn.String, Value: v} }
	series := learn.Series{Key: "update jobs", Values: map[string]learn.Leaf{
		"body.q._id": leaf("body.q._id", "job-rec"),
		"body.u.by":  leaf("body.u.by", "job-rec"),
		"body.u.st":  leaf("body.u.st", "RUNNING"),
	}}
	call := func(id, by, st string) learn.Shape {
		return learn.Shape{Key: "update jobs", Leaves: map[string]learn.Leaf{
			"body.q._id": leaf("body.q._id", id), "body.u.by": leaf("body.u.by", by), "body.u.st": leaf("body.u.st", st)}}
	}
	bind := func(name, rec, live string) variables.Binding {
		return variables.Binding{Name: name, Recorded: rec, Value: live}
	}
	own := MarkedSeries{Series: series, Owner: "T1"}
	for _, c := range []struct {
		name    string
		m       MarkedSeries
		live    learn.Shape
		inForce []variables.Binding
		shape   bool   // the series' shape holds asked
		moved   string // joined
		off     string // a part of it; "" for none
	}{
		{"this run's id is given back", own, call("job-live", "job-live", "RUNNING"), []variables.Binding{bind("v1", "job-rec", "job-live")}, true, "job-rec", ""},
		{"a set-wide series reads no variable", MarkedSeries{Series: series}, call("job-live", "job-live", "RUNNING"), []variables.Binding{bind("v1", "job-rec", "job-live")}, false, "", ""},
		{"no binding: compared as recorded", own, call("job-live", "job-live", "RUNNING"), nil, false, "", ""},
		{"no binding of that id: compared as recorded", own, call("job-live", "job-live", "RUNNING"), []variables.Binding{bind("v1", "other-rec", "job-live")}, false, "", ""},
		{"the recorded id while the run bound another", own, call("job-rec", "job-rec", "RUNNING"), []variables.Binding{bind("v1", "job-rec", "job-live")}, true, "", "it carries the recorded id job-rec at body.q._id, but T1's window bound that id to job-live"},
		{"a third id", own, call("job-3rd", "job-3rd", "RUNNING"), []variables.Binding{bind("v1", "job-rec", "job-live")}, true, "", "it carries job-3rd at body.q._id, where T1's window bound the id recorded as job-rec to job-live"},
		{"one place on the id, one beside it", own, call("job-live", "job-3rd", "RUNNING"), []variables.Binding{bind("v1", "job-rec", "job-live")}, true, "job-rec", "it carries job-3rd at body.u.by"},
		{"two bindings, two values", own, call("job-live", "job-live", "RUNNING"), []variables.Binding{bind("v1", "job-rec", "job-live"), bind("v2", "job-rec", "job-2nd")}, true, "", "two variables recorded as job-rec are bound to two values this run (job-live, job-2nd)"},
		{"two bindings, one value", own, call("job-live", "job-live", "RUNNING"), []variables.Binding{bind("v1", "job-rec", "job-live"), bind("v2", "job-rec", "job-live")}, true, "job-rec", ""},
		{"an id that replays the same", own, call("job-rec", "job-rec", "RUNNING"), []variables.Binding{bind("v1", "job-rec", "job-rec")}, true, "", ""},
		{"another value beside the id still differs", own, call("job-live", "job-live", "STOPPED"), []variables.Binding{bind("v1", "job-rec", "job-live")}, false, "job-rec", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := c.live.Leaves["body.q._id"].Value
			asked, moved, off := c.m.OwnCall(c.live, c.inForce)
			if got := series.HasShape(asked, learn.None); got != c.shape {
				t.Errorf("HasShape(asked) = %v; want %v (asked %v)", got, c.shape, asked.Leaves)
			}
			if got := strings.Join(moved, ","); got != c.moved {
				t.Errorf("moved = %q; want %q", got, c.moved)
			}
			if c.off == "" && off != "" || c.off != "" && !strings.Contains(off, c.off) {
				t.Errorf("off = %q; want it to say %q", off, c.off)
			}
			if c.live.Leaves["body.q._id"].Value != before {
				t.Errorf("OwnCall wrote into live's leaves")
			}
		})
	}
}
