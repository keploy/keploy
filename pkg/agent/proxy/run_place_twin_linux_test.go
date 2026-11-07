//go:build linux

package proxy

import (
	"slices"
	"testing"
	"time"
)

// placedAnswered is the answer got gives while its test still runs: the row
// fails if it is not written within 10 s, as a call its test waits for would
// then hold that test until its timeout.
func placedAnswered(t *testing.T, got <-chan string, what string) string {
	t.Helper()
	select {
	case a := <-got:
		return a
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no answer while the test runs; a test waiting for this call could not end", what)
		return ""
	}
}

// Two identical calls of one test: on record the one its test waited for
// was answered inside the test, the other (a background refresh of the same
// thing) after the test, in test-3. On replay either call can reach the
// proxy first and the matcher gives it either recording, so neither answer
// is held: both are written while test-1 runs, in each order on record,
// with the calls one after the other and both at once.
func TestHeldAnswer_IdenticalCallsOfOneTestAreAnsweredInIt(t *testing.T) {
	for _, tc := range []struct {
		name              string
		bgFirst, together bool
	}{
		{"the background call recorded first, the calls one by one", true, false},
		{"the awaited call recorded first, the calls one by one", false, false},
		{"the background call recorded first, both calls at once", true, true},
		{"the awaited call recorded first, both calls at once", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bothSwitches(t, func(t *testing.T, proven bool) {
				bgReq, fgReq := placeAt(2), placeAt(1)
				if tc.bgFirst {
					bgReq, fgReq = placeAt(1), placeAt(2)
				}
				s := stagePlacedSet(t, proven, placeWindows(),
					placedMock("mock-bg", "/same", bgReq, placeAt(45)),
					placedMock("mock-fg", "/same", fgReq, placeAt(3)))
				s.open(t, "test-1", "mock-bg", "mock-fg")
				first := s.call(t, "/same")
				var second <-chan string
				if tc.together {
					second = s.call(t, "/same")
				}
				a := placedAnswered(t, first, "the first call")
				if !tc.together {
					second = s.call(t, "/same")
				}
				b := placedAnswered(t, second, "the second call")
				got := []string{a, b}
				slices.Sort(got)
				if !slices.Equal(got, []string{"mock-bg", "mock-fg"}) {
					t.Fatalf("the two calls read %q, want each recording once", got)
				}
				s.end(t, "test-1")
			})
		})
	}
}

// Two identical calls of one test whose answers both came after the test on
// record: the test waited for neither, so whichever call takes whichever
// recording, each answer is held to its own place, test-3.
func TestHeldAnswer_IdenticalCallsBothAnsweredAfterTheirTestAreHeld(t *testing.T) {
	bothSwitches(t, func(t *testing.T, proven bool) {
		s := stagePlacedSet(t, proven, placeWindows(),
			placedMock("mock-a", "/same", placeAt(1), placeAt(45)),
			placedMock("mock-b", "/same", placeAt(2), placeAt(46)))
		s.open(t, "test-1", "mock-a", "mock-b")
		a, b := s.call(t, "/same"), s.call(t, "/same")
		s.end(t, "test-1")
		placedNotYet(t, a, "test-3's answer after test-1")
		placedNotYet(t, b, "test-3's answer after test-1")
		s.open(t, "test-2")
		s.end(t, "test-2")
		placedNotYet(t, a, "test-3's answer after test-2")
		placedNotYet(t, b, "test-3's answer after test-2")
		s.open(t, "test-3")
		got := []string{placedAnswered(t, a, "at test-3's open"), placedAnswered(t, b, "at test-3's open")}
		slices.Sort(got)
		if !slices.Equal(got, []string{"mock-a", "mock-b"}) {
			t.Fatalf("the two calls read %q, want each recording once", got)
		}
		s.end(t, "test-3")
	})
}

// An identical recording of another test, answered inside that test, is no
// twin of test-1's: test-1's background answer is still held to test-3,
// and test-2's own call is answered in test-2.
func TestHeldAnswer_AnIdenticalCallOfAnotherTestKeepsTheHold(t *testing.T) {
	bothSwitches(t, func(t *testing.T, proven bool) {
		s := stagePlacedSet(t, proven, placeWindows(),
			placedMock("mock-held", "/same", placeAt(2), placeAt(45)),
			placedMock("mock-own2", "/same", placeAt(22), placeAt(23)))
		s.open(t, "test-1", "mock-held")
		got := s.call(t, "/same")
		s.end(t, "test-1")
		placedNotYet(t, got, "test-3's answer after test-1")
		s.open(t, "test-2", "mock-own2")
		placedWritten(t, s.call(t, "/same"), "mock-own2", "test-2's own call")
		placedNotYet(t, got, "test-3's answer in test-2")
		s.end(t, "test-2")
		s.open(t, "test-3")
		placedWritten(t, got, "mock-held", "at test-3's open")
		s.end(t, "test-3")
	})
}

// Another call of the same test, answered inside it, is no twin of a call
// it does not match: test-1's answer recorded in test-3 is still held while
// test-1's own other call is answered at once.
func TestHeldAnswer_AnotherCallOfTheTestKeepsTheHold(t *testing.T) {
	bothSwitches(t, func(t *testing.T, proven bool) {
		s := stagePlacedSet(t, proven, placeWindows(),
			placedMock("mock-held", "/held", placeAt(2), placeAt(45)),
			placedMock("mock-other", "/other", placeAt(3), placeAt(4)))
		s.open(t, "test-1", "mock-held", "mock-other")
		got := s.call(t, "/held")
		placedWritten(t, s.call(t, "/other"), "mock-other", "test-1's other call")
		s.end(t, "test-1")
		placedNotYet(t, got, "test-3's answer after test-1")
		s.open(t, "test-2")
		s.end(t, "test-2")
		s.open(t, "test-3")
		placedWritten(t, got, "mock-held", "at test-3's open")
		s.end(t, "test-3")
	})
}
