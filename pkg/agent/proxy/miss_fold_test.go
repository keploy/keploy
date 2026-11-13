package proxy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// pollerMiss is the miss of a poller's call at second s: what a poller sends
// differs from call to call only in its values (the order's id, the time in
// its body), so its rendered request, diff and field values do too.
func pollerMiss(s int) models.ParserError {
	at := time.Date(2026, 11, 13, 14, 25, s, 0, time.UTC)
	return models.ParserError{
		ParserErrorType: models.ErrMockNotFound,
		MismatchReport: &models.MockMismatchReport{
			At: at, Protocol: "HTTP", ActualSummary: fmt.Sprintf("GET /orders/%d", 100+s), Destination: "orders:8080",
			ClosestMock: "mock-orders", MatchPhase: "body", NextSteps: "re-record the poller's test",
			ReceivedReq: fmt.Sprintf("GET /orders/%d\n\n{\"since\":%q}", 100+s, at),
			Diff:        fmt.Sprintf("body.since: %q", at),
			FieldDiffs:  []models.MockFieldDiff{{Path: "body.since", Expected: "2026-11-01", Actual: at.String()}},
			Differs:     []string{"body.since"},
		},
	}
}

// distinctMiss is a miss alike to no other distinctMiss: its closest recording
// is its own.
func distinctMiss(i int) models.ParserError {
	return models.ParserError{
		ParserErrorType: models.ErrMockNotFound,
		MismatchReport:  &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /other", ClosestMock: fmt.Sprintf("mock-%d", i)},
	}
}

// shapeName is a path segment for i that its shape keeps (models.PathShape:
// letters only, none of them a-f), so calls to it are alike to no call to
// another i's.
func shapeName(i int) string {
	name := []byte{'x'}
	for ; i > 0; i /= 20 {
		name = append(name, byte('g'+i%20))
	}
	return string(name)
}

// heldTimes is the times folded into the miss err holds.
func heldTimes(t *testing.T, err error) []time.Time {
	t.Helper()
	pe, ok, _ := filedMiss(err)
	if !ok {
		t.Fatalf("%v holds no miss", err)
	}
	return pe.MismatchReport.AlikeAt
}

// A poller that keeps missing past its window's room loses no call: each one
// alike to a miss the window holds is kept with it by its time, and the read
// lists each as a miss of its own, right after the one it is alike to, so
// it is complete. The calls folded say they are (Folded): their rendered
// request and diff are the first's.
func TestAFloodOfOneCallPastTheRoomIsKeptCallByCall(t *testing.T) {
	eitherSwitch(t, func(t *testing.T) {
		p := startedDrainProxy(t, maxPendingMockErrors+8)
		p.BeginTestErrorCapture()
		p.SendError(pollerMiss(0))
		for i := 1; i < maxPendingMockErrors; i++ {
			p.SendError(distinctMiss(i))
		}
		for s := 1; s <= 3; s++ {
			p.SendError(pollerMiss(s))
		}
		got, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatalf("GetMockErrors = %v; every call past the room was alike to a held miss, so none was lost", err)
		}
		if len(got) != maxPendingMockErrors+3 {
			t.Fatalf("read %d misses; want the %d held and the 3 folded", len(got), maxPendingMockErrors)
		}
		if got[0].Folded || got[0].At != pollerMiss(0).MismatchReport.At {
			t.Fatalf("the first poller miss reads %+v; want it as it was made", got[0])
		}
		for s := 1; s <= 3; s++ {
			c := got[s]
			if !c.Folded || c.At != pollerMiss(s).MismatchReport.At || c.ClosestMock != "mock-orders" || c.ReceivedReq != got[0].ReceivedReq {
				t.Fatalf("call %d reads Folded %v at %v (closest %q); want it folded, at its own time, the rest the first's", s, c.Folded, c.At, c.ClosestMock)
			}
		}
		if got[4].ClosestMock != "mock-1" || got[4].Folded {
			t.Fatalf("the miss after the folded calls is %+v; want the next held one", got[4])
		}
	})
}

// A miss made before the set's first test that folds into one held is read
// as that one is: made before the set.
func TestAFoldedMissMadeBeforeTheSetIsReadAsTheSets(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 8)
	p.SendError(pollerMiss(0))
	for i := 1; i < maxPendingMockErrors; i++ {
		p.SendError(distinctMiss(i))
	}
	p.SendError(pollerMiss(1))
	flushDrain(p)
	p.BeginTestErrorCapture()
	got, err := p.GetMockErrors(context.Background())
	if err != nil || len(got) != maxPendingMockErrors+1 {
		t.Fatalf("the set's first test read %d misses, err %v; want all %d, complete", len(got), err, maxPendingMockErrors+1)
	}
	if !got[1].Folded || !got[1].BeforeSet || got[1].Kind != models.MissOutsideTest {
		t.Fatalf("the folded call reads %+v; want it read as made before the set", got[1])
	}
}

// Which calls are alike (missAlikeKey): only a call's time and the values it
// carried may differ. Anything else the read says of a miss, or that places
// it at a test's end, keeps it apart, and a miss alike to none past its
// list's room is dropped as before (the read says it is incomplete).
func TestOnlyACallsTimeAndValuesMayDifferForItToFold(t *testing.T) {
	type change func(pe *models.ParserError, r *models.MockMismatchReport) error
	same := func(pe *models.ParserError, _ *models.MockMismatchReport) error { return *pe }
	rows := []struct {
		name string
		fold bool
		edit change
	}{
		{"its time, request, diff and values", true, same},
		{"its path's value in the same shape", true, func(pe *models.ParserError, r *models.MockMismatchReport) error {
			r.ActualSummary = "GET /orders/999"
			return *pe
		}},
		{"times folded into it before", true, func(pe *models.ParserError, r *models.MockMismatchReport) error {
			r.AlikeAt = []time.Time{time.Unix(9, 0)}
			return *pe
		}},
		{"another path shape", false, func(pe *models.ParserError, r *models.MockMismatchReport) error {
			r.ActualSummary = "GET /users/7"
			return *pe
		}},
		{"another closest recording", false, func(pe *models.ParserError, r *models.MockMismatchReport) error {
			r.ClosestMock = "mock-users"
			return *pe
		}},
		{"another phase", false, func(pe *models.ParserError, r *models.MockMismatchReport) error {
			r.MatchPhase = "header"
			return *pe
		}},
		{"another field differing", false, func(pe *models.ParserError, r *models.MockMismatchReport) error {
			r.FieldDiffs[0].Path = "body.until"
			return *pe
		}},
		{"another next step", false, func(pe *models.ParserError, r *models.MockMismatchReport) error {
			r.NextSteps = "add the call"
			return *pe
		}},
		{"another test worker", false, func(pe *models.ParserError, _ *models.MockMismatchReport) error {
			pe.Instance = 2
			return *pe
		}},
		{"another kind", false, func(pe *models.ParserError, _ *models.MockMismatchReport) error {
			pe.MissKind = models.MissExtraCall
			return *pe
		}},
		{"another recorded count", false, func(pe *models.ParserError, _ *models.MockMismatchReport) error {
			pe.Recorded = 2
			return *pe
		}},
		{"made while staging", false, func(pe *models.ParserError, _ *models.MockMismatchReport) error {
			pe.Staging = true
			return *pe
		}},
		{"marked at another boundary", false, func(pe *models.ParserError, r *models.MockMismatchReport) error {
			r.MarkRound, r.MarkBounds = true, 3
			return *pe
		}},
		{"marked in another test's view", false, func(pe *models.ParserError, r *models.MockMismatchReport) error {
			r.MarkedTest = "test-2"
			return *pe
		}},
		{"made before its set", false, func(pe *models.ParserError, _ *models.MockMismatchReport) error {
			return asBeforeSet(*pe)
		}},
		{"made in an earlier set", false, func(pe *models.ParserError, _ *models.MockMismatchReport) error {
			return asEarlierSet(*pe)
		}},
		{"another parser error", false, func(pe *models.ParserError, _ *models.MockMismatchReport) error {
			pe.ParserErrorType = "other"
			return *pe
		}},
		{"no report", false, func(pe *models.ParserError, _ *models.MockMismatchReport) error {
			pe.MismatchReport = nil
			return *pe
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			a := &testErrorAccumulator{}
			if !a.addBounded(pollerMiss(0), 1) {
				t.Fatal("the first miss was not held")
			}
			pe := pollerMiss(5)
			r := *pe.MismatchReport
			r.FieldDiffs = append([]models.MockFieldDiff(nil), r.FieldDiffs...)
			pe.MismatchReport = &r
			if kept := a.addBounded(row.edit(&pe, &r), 1); kept != row.fold {
				t.Fatalf("a miss past the room differing in %s was kept %v; want %v", row.name, kept, row.fold)
			}
			held := a.drain()
			if len(held) != 1 {
				t.Fatalf("the list holds %d; want the one it had room for", len(held))
			}
			if n := len(heldTimes(t, held[0])); row.fold != (n > 0) {
				t.Fatalf("the held miss carries %d folded times; folded %v", n, row.fold)
			}
		})
	}
}

// A miss moved from one list to another (made between two tests, carried into
// the next test's window) brings the times folded into it: folded again,
// each joins the held miss's, after its own time.
func TestAMovedMissFoldsWithTheTimesFoldedIntoIt(t *testing.T) {
	a := &testErrorAccumulator{}
	a.addBounded(pollerMiss(0), 1)
	moved := withAlikeAt(pollerMiss(1), []time.Time{pollerMiss(2).MismatchReport.At})
	if !a.addBounded(moved, 1) {
		t.Fatal("the moved miss was not folded")
	}
	got := heldTimes(t, a.drain()[0])
	want := []time.Time{pollerMiss(1).MismatchReport.At, pollerMiss(2).MismatchReport.At}
	if len(got) != 2 || !got[0].Equal(want[0]) || !got[1].Equal(want[1]) {
		t.Fatalf("the held miss carries %v; want %v", got, want)
	}
}

// A miss folds into the first held miss it is alike to, and a drained list
// starts with nothing folded: its next misses are held as made.
func TestAMissFoldsIntoTheFirstAlikeAndADrainForgetsTheFolds(t *testing.T) {
	a := &testErrorAccumulator{}
	a.addBounded(pollerMiss(0), 2)
	a.addBounded(pollerMiss(1), 2)
	a.addBounded(pollerMiss(2), 2)
	held := a.drain()
	if n0, n1 := len(heldTimes(t, held[0])), len(heldTimes(t, held[1])); n0 != 1 || n1 != 0 {
		t.Fatalf("folded %d into the first alike and %d into the second; want 1 and 0", n0, n1)
	}
	a.addBounded(pollerMiss(3), 2)
	if held = a.drain(); len(held) != 1 || len(heldTimes(t, held[0])) != 0 {
		t.Fatalf("after a drain the list holds %d, the first with times %v; want one miss, as made", len(held), heldTimes(t, held[0]))
	}
}

// An answered call's report past its room is dropped, never folded: its
// count is the report's, and no verdict reads it (noteAnswerDropped).
func TestAnAnswerPastItsRoomIsNotFolded(t *testing.T) {
	answer := func() models.ParserError {
		pe := pollerMiss(0)
		pe.MismatchReport.Answer = models.AnswerByKind
		return pe
	}
	a := &testErrorAccumulator{}
	a.addBounded(answer(), 1)
	if a.addBounded(answer(), 1) {
		t.Fatal("an answer past its room was kept")
	}
	if n := len(heldTimes(t, a.drain()[0])); n != 0 {
		t.Fatalf("the held answer carries %d folded times; want none", n)
	}
}

// A call folded into another is listed as a call of its own: changing what
// one copy binds never changes another's.
func TestFoldedCallsShareNoList(t *testing.T) {
	first := models.UnmatchedCall{BoundIn: []string{"a"}, BackgroundBound: []models.BackgroundBinding{{}}, Repeat: &models.RepeatNote{Mock: "mock-1"}}
	calls := appendFolded([]models.UnmatchedCall{first}, []time.Time{time.Unix(1, 0), time.Unix(2, 0)})
	calls[1].BoundIn[0] = "b"
	if calls[0].BoundIn[0] != "a" || calls[2].BoundIn[0] != "a" || &calls[1].BackgroundBound[0] == &calls[0].BackgroundBound[0] || calls[1].Repeat == calls[0].Repeat {
		t.Fatalf("the folded calls share their lists: %+v", calls)
	}
}

// A held miss filed as made before its set, or in an earlier set, is filed so
// still once misses fold into it; and the report its parser made is not
// changed by a fold (the times are on a copy).
func TestAFoldKeepsHowTheHeldMissIsFiled(t *testing.T) {
	for name, wrap := range map[string]func(error) error{"made before its set": asBeforeSet, "made in an earlier set": asEarlierSet} {
		t.Run(name, func(t *testing.T) {
			orig := pollerMiss(0)
			a := &testErrorAccumulator{}
			a.addBounded(wrap(orig), 1)
			if !a.addBounded(wrap(pollerMiss(1)), 1) {
				t.Fatal("an alike miss filed the same way was not folded")
			}
			held := a.drain()[0]
			_, _, marks := filedMiss(held)
			_, _, want := filedMiss(wrap(orig))
			if len(marks) != 1 || marks[0] != want[0] || len(heldTimes(t, held)) != 1 {
				t.Fatalf("the held miss is filed %v with %d folded times; want %v with 1", marks, len(heldTimes(t, held)), want)
			}
			if orig.MismatchReport.AlikeAt != nil {
				t.Fatalf("the parser's report was changed: %v", orig.MismatchReport.AlikeAt)
			}
		})
	}
}
