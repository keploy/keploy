package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// olderSetAside is what a value refused past a payload record's full round
// gave the application only in an older test of the set says of it, after
// its name.
const olderSetAside = "only in an older test of the set, and what it gave there shows nothing of what the application did with the value in the tests since, so keploy cannot tell a copy from a varying value"

// No excuse from a place served only in an older test (a founder ruling,
// 2026-11-02). Under the rule a copy follows its source, a payload F gave the
// application as recorded, or whose view shows it gave another value there
// that the value took none of the words of, lets a word of a drifted value go
// as one the value did not copy there. A payload F gave the application only
// in an older test of the set than the call's (Raw.InAnOlderTest) shows
// nothing of what the application did with that word in the tests since, so
// no noise stands on it: in each of the rule's three steps the verdict that
// was noise is refused, naming the payload and why (fail closed), and the
// same payload F gave anywhere else stays noise. The search for the value's
// source still goes past it, as it did, so a place after it stays where the
// value came from, and a value its call's window learns as a read of the
// clock, which every replay checks, is learned so
// (TestClassifyFollowing_AnOlderTestsAnswerTheValueMovedApartFromIsNoSource).
func TestAPlaceServedOnlyInAnOlderTestExcusesNoWord(t *testing.T) {
	before := eRecAt.Add(-150 * time.Millisecond)
	older := func(raw []Raw) []Raw {
		out := make([]Raw, len(raw))
		for i, x := range raw {
			out[i] = x.ServedInAnOlderTest()
		}
		return out
	}
	// The value whole at a payload F sent unchanged (refute, the word
	// search): Flyway's execution_time recorded as 50, a literal of the
	// history table's CREATE TABLE (VARCHAR(50)), bound 7 in F.
	history := append(eStartup(), eHistory(before, 6, true))
	if got := classifyF(t, "50", "7", nil, history, nil); got.Kind != KindNoise {
		t.Fatalf("50 beside VARCHAR(50) F sent unchanged in the call's own test: %s (%q), want noise", got.Kind, got.Why)
	}
	if got := classifyF(t, "50", "7", nil, older(history), nil); got.Kind != KindUnaccounted || !strings.Contains(got.Why, "in mock-5 at") || !strings.Contains(got.Why, "record's full round gave the application mock-5 "+olderSetAside) {
		t.Errorf("FALSE GREEN? 50 beside VARCHAR(50) F sent unchanged only in an older test: %s (%q), want refused naming mock-5 and why", got.Kind, got.Why)
	}
	// A word of the value at a payload F gave as recorded (excused):
	// execution_time 82 at record, 1 in F, which "SELECT 1" and "INSERT 0 1"
	// hold.
	if got := classifyF(t, "82", "1", nil, eStartup(), nil); got.Kind != KindNoise {
		t.Fatalf("execution_time 82 at record, 1 in F, beside the call's own test's SELECT 1: %s (%q), want noise", got.Kind, got.Why)
	}
	if got := classifyF(t, "82", "1", nil, older(eStartup()), nil); got.Kind != KindUnaccounted || !strings.Contains(got.Why, "in mock-1 at") || !strings.Contains(got.Why, "record's full round gave the application mock-1 "+olderSetAside) {
		t.Errorf("FALSE GREEN? execution_time 82 at record, 1 in F, beside SELECT 1 F gave only in an older test: %s (%q), want refused naming mock-1 and why", got.Kind, got.Why)
	}
	// A word of the value at a payload F gave its view of (excusedLive): the
	// call's random appId holds "48d1" of the id F gave in an earlier answer
	// by chance, where the recording gave another id that the recorded value
	// took none of the words of.
	const (
		p      = "body.appId"
		recID  = "bb7ed9ee-670e-4aa1-982f-b54b2b73982f"
		liveID = "e1f2a3b4-c5d6-48d1-8a9b-0c1d2e3f4a5b"
		chance = "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812"
		other  = "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5"
	)
	at := r1Insert.Add(-time.Minute)
	src := func(id string) *models.Mock {
		m := receiveOf("mock-31", id, at)
		m.Spec.ResTimestampMock = at.Add(time.Second)
		return m
	}
	for _, inOlder := range []bool{false, true} {
		dc := clockedDrifts()
		x := dc.RawOf(src(recID), true).ServedAs(3).ServedLive(dc.RawOf(src(liveID), true))
		if inOlder {
			x = x.ServedInAnOlderTest()
		}
		got := ClassifyFollowing(dc, []string{p}, strLeaf(p, chance), strLeaf(p, other), r1Insert, nil, []Raw{x}, nil, nil, nil)[0]
		switch {
		case !inOlder && got.Kind != KindNoise:
			t.Fatalf("48d1 of the id F gave in the call's own test, by chance: %s (%q), want noise", got.Kind, got.Why)
		case inOlder && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, "in mock-31 at") || !strings.Contains(got.Why, "record's full round gave the application mock-31 "+olderSetAside)):
			t.Errorf("FALSE GREEN? 48d1 of the id F gave only in an older test, by chance: %s (%q), want refused naming mock-31 and why", got.Kind, got.Why)
		}
	}
}
