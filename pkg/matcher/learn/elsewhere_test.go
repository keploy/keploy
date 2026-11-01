package learn

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// One pairing per recording per round, and its own one wins: a
// recording a call of the round consumed keeps what its own pairing learned
// alone, and what a call made in another window taught it is voided, never
// merged, whichever came first (the agent pairs no consumed recording across
// windows, so a teaching beside a consumption came first). A recording no
// call consumed carries its teaching, and two states the agent sends for one
// such recording (two teachings of one kind) are merged, never overwritten.
// The states handed in are not changed.
func TestARecordingsOwnPairingVoidsWhatACallInAnotherWindowTaughtIt(t *testing.T) {
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	second := models.ClockOffset{Min: -time.Second, Max: -time.Second, Res: time.Millisecond}
	consumed := []models.MockState{{Name: "mock-1", Clocks: map[string]models.ClockOffset{"own_at": minute}}, {Name: "mock-2"}}
	elsewhere := []models.MockState{
		{Name: "mock-1", Clocks: map[string]models.ClockOffset{"claimed_at": second}},
		{Name: "mock-2", Clocks: map[string]models.ClockOffset{"claimed_at": second}},
		{Name: "mock-9", Clocks: map[string]models.ClockOffset{"claimed_at": second}},
		{Name: "mock-9", Clocks: map[string]models.ClockOffset{"expires_at": minute}},
	}
	kept, voided := OwnPairingWins(func(name string) bool { return name == "mock-1" || name == "mock-2" }, elsewhere)
	if len(voided) != 2 || voided[0].Name != "mock-1" || voided[1].Name != "mock-2" || len(kept) != 2 || kept[0].Name != "mock-9" || kept[1].Name != "mock-9" {
		t.Fatalf("kept %v, voided %v; want mock-1 and mock-2 (consumed in the round) voided, mock-9's two teachings kept", learnedStateNames(kept), learnedStateNames(voided))
	}
	want := []models.MockState{
		{Name: "mock-1", Clocks: map[string]models.ClockOffset{"own_at": minute}},
		{Name: "mock-2"},
		{Name: "mock-9", Clocks: map[string]models.ClockOffset{"claimed_at": second, "expires_at": minute}},
	}
	for _, from := range [][]models.MockState{elsewhere, kept} {
		if out := WithLearnedElsewhere(consumed, from); !reflect.DeepEqual(out, want) {
			t.Fatalf("written %+v; want %+v: mock-1 with its own clock read alone, mock-2 as consumed, mock-9 with both teachings", out, want)
		}
	}
	if len(consumed) != 2 || !reflect.DeepEqual(consumed[0].Clocks, map[string]models.ClockOffset{"own_at": minute}) || consumed[1].Clocks != nil {
		t.Fatalf("the consumed states changed: %+v", consumed)
	}
	if len(elsewhere[2].Clocks) != 1 {
		t.Fatalf("the states the agent sent changed: %+v", elsewhere[2])
	}
}

func learnedStateNames(sts []models.MockState) []string {
	out := make([]string, 0, len(sts))
	for _, st := range sts {
		out = append(out, st.Name)
	}
	return out
}

// Learning is scoped to a window: a window keploy did not know the test of
// is read as any window's, and no tests means none.
func TestWhatAWindowTaughtIsReadAsThatWindowsOrAnyWhenUnknown(t *testing.T) {
	inC := []models.TeachingWindow{{Test: "o::C"}}
	between := []models.TeachingWindow{{Test: "o::D"}, {}}
	for _, c := range []struct {
		ws    []models.TeachingWindow
		tests map[string]bool
		want  bool
	}{
		{inC, map[string]bool{"o::C": true}, true},
		{inC, map[string]bool{"o::A": true}, false},
		{between, map[string]bool{"o::A": true}, true},
		{between, nil, false},
		{nil, map[string]bool{"o::A": true}, false},
	} {
		if got := TaughtInAny(c.ws, c.tests); got != c.want {
			t.Errorf("TaughtInAny(%+v, %v) = %v; want %v", c.ws, c.tests, got, c.want)
		}
	}
	if got := SayWindows([]models.TeachingWindow{{Test: "o::C"}, {}}); got != "o::C's window, between tests" {
		t.Fatalf("SayWindows said %q", got)
	}
}

// A recording is said as its owner test's first, then as the set's worker
// scope's when worker names it, else as the set's start-up and pool's: a
// mapping that lists one recording under a test and under worker: says the
// test, and a nil worker names no recording as the worker scope's.
func TestOwnerSaidNamesTheOwnerTestFirst(t *testing.T) {
	owner := map[string]string{"mock-1": "o::A"}
	worker := func(name string) bool { return name == "mock-1" || name == "mock-w" }
	for _, c := range []struct {
		name   string
		worker func(string) bool
		want   string
	}{
		{"mock-1", worker, "o::A"},
		{"mock-w", worker, "the set's worker scope"},
		{"mock-p", worker, "the set's start-up and pool"},
		{"mock-w", nil, "the set's start-up and pool"},
	} {
		if got := OwnerSaid(c.name, owner, c.worker); got != c.want {
			t.Fatalf("OwnerSaid(%s) = %q; want %q", c.name, got, c.want)
		}
	}
}
