package learn

import (
	"slices"
	"testing"
	"time"
)

// TestAtWindowCloseCallsOnceAtTheNextBoundary: a call AtWindowClose holds is
// made once, at the first of End, Begin and NewSet after it was given, before
// that call returns, without the tracker's lock (it reads the tracker), in
// the order given; never again at a later boundary. stop reports whether it
// took the call back before the window closed.
func TestAtWindowCloseCallsOnceAtTheNextBoundary(t *testing.T) {
	boundaries := map[string]func(tr *Tracker){
		"End":             func(tr *Tracker) { tr.End() },
		"Begin":           func(tr *Tracker) { tr.Begin() },
		"BeginAwaiting":   func(tr *Tracker) { tr.BeginAwaiting() },
		"NewSet":          func(tr *Tracker) { tr.NewSet() },
		"BeginAt":         func(tr *Tracker) { tr.BeginAt(time.Now()) },
		"BeginAwaitingAt": func(tr *Tracker) { tr.BeginAwaitingAt(time.Now()) },
	}
	for name, closeIt := range boundaries {
		t.Run(name, func(t *testing.T) {
			tr := NewTracker()
			tr.Begin()
			var calls []string
			var openAt []bool
			stopA := tr.AtWindowClose(func() {
				calls = append(calls, "a")
				openAt = append(openAt, tr.Open()) // takes t.mu: no deadlock
			})
			stopB := tr.AtWindowClose(func() { calls = append(calls, "b") })
			if len(calls) != 0 {
				t.Fatalf("called before the window closed: %v", calls)
			}
			closeIt(tr)
			if !slices.Equal(calls, []string{"a", "b"}) {
				t.Fatalf("calls at %s = %v, want [a b], once each, before it returned", name, calls)
			}
			if name == "End" || name == "NewSet" {
				if openAt[0] {
					t.Errorf("at %s the call read a test open; want the window it closes closed", name)
				}
			}
			if stopA() || stopB() {
				t.Errorf("stop after the window closed reports the call taken back; want false")
			}
			tr.End()
			tr.Begin()
			tr.NewSet()
			if len(calls) != 2 {
				t.Errorf("a later boundary called again: %v", calls)
			}
		})
	}
}

// TestAtWindowCloseStopTakesTheCallBack: stop before the window closes takes
// the call back (true), and the boundary makes none of it; the other calls
// held are made as given. A second stop reports false: nothing is held.
func TestAtWindowCloseStopTakesTheCallBack(t *testing.T) {
	tr := NewTracker()
	tr.Begin()
	var calls []string
	stopA := tr.AtWindowClose(func() { calls = append(calls, "a") })
	tr.AtWindowClose(func() { calls = append(calls, "b") })
	stopC := tr.AtWindowClose(func() { calls = append(calls, "c") })
	if !stopA() {
		t.Fatal("stop before the window closed reports false; want true")
	}
	if stopped(t, func() func() bool { return stopA }) {
		t.Error("a second stop reports true; want false: nothing is held")
	}
	tr.End()
	if !slices.Equal(calls, []string{"b", "c"}) {
		t.Errorf("calls = %v, want [b c]", calls)
	}
	if stopC() {
		t.Error("stop after the call was made reports true")
	}
}

// TestAtWindowCloseBetweenTests: a call given while no test is open (after
// End, before the next Begin) is made at that Begin, the window's close.
func TestAtWindowCloseBetweenTests(t *testing.T) {
	tr := NewTracker()
	tr.Begin()
	tr.End()
	n := 0
	tr.AtWindowClose(func() { n++ })
	tr.End() // no test is open, but End is a boundary all the same
	if n != 1 {
		t.Fatalf("a second End made %d calls; want 1: every End closes the window", n)
	}
	tr.AtWindowClose(func() { n++ })
	tr.Begin()
	if n != 2 {
		t.Errorf("Begin after End made %d calls in all; want 2", n)
	}
}

// TestAtWindowCloseNilTracker: a nil tracker holds nothing, and its stop
// reports true.
func TestAtWindowCloseNilTracker(t *testing.T) {
	var tr *Tracker
	if !stopped(t, func() func() bool { return tr.AtWindowClose(func() { t.Error("called") }) }) {
		t.Error("a nil tracker's stop reports false; want true")
	}
}

// stopped is the answer of the stop hold gives, false where either panics
// (a row's failure, not the binary's crash).
func stopped(t *testing.T, hold func() func() bool) (ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panicked: %v", r)
			ok = false
		}
	}()
	return hold()()
}
