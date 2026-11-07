//go:build linux

package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A recording's answer is held at most once in a set: a serve of it again
// (a stateful cursor saturated on it) is written at once. A new set's
// staging claims afresh.
func TestRunPlace_ClaimHoldOncePerStaging(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a claim panicked: %v", r)
		}
	}()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	if !mm.ClaimHold("mock-a") || mm.ClaimHold("mock-a") {
		t.Fatal("before any staging: the first hold is not the one claimed")
	}
	mm.SeedRecordedWindows(placeWindows())
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, placeAt(3600))
	if !mm.ClaimHold("mock-a") {
		t.Fatal("the first hold of a recording is refused")
	}
	if mm.ClaimHold("mock-a") {
		t.Fatal("a second hold of one recording in one set is claimed")
	}
	if !mm.ClaimHold("mock-b") {
		t.Fatal("another recording's first hold is refused")
	}
	if mm.ClaimHold("mock-a") {
		t.Fatal("another recording's claim gave the first one's hold back")
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, placeAt(3600))
	if !mm.ClaimHold("mock-a") {
		t.Fatal("the next set's first hold of the recording is refused")
	}
}

// The open test's recordings are those it was staged with, a recording a
// call took included: a test's window's own with the switch off, the test's
// own under it, and none at a set's staging.
func TestRunPlace_OpenTestRecordingsKeepWhatACallTook(t *testing.T) {
	names := func(ms []*models.Mock) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Name)
		}
		return out
	}
	bothSwitches(t, func(t *testing.T, proven bool) {
		sw := ""
		if proven {
			sw = "1"
		}
		t.Setenv(provenswitch.Env, sw)
		a := placedMock("mock-a", "/a", placeAt(2), placeAt(3))
		b := placedMock("mock-b", "/b", placeAt(22), placeAt(23))
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("a store never staged: %v", r)
				}
			}()
			if got := mm.OpenTestRecordings(); len(got) != 0 {
				t.Fatalf("a store never staged: %v, want none", names(got))
			}
		}()
		mm.SeedRecordedWindows(placeWindows())
		mm.SetMocksWithWindow([]*models.Mock{a, b}, nil, models.BaseTime, placeAt(3600))
		if got := mm.OpenTestRecordings(); len(got) != 0 {
			t.Fatalf("a set's staging: %v, want none", names(got))
		}
		open := func(test string, w models.TestWindow, own *models.Mock) {
			if proven {
				mm.ServeExactTest(test, []*models.Mock{roundMock(mm, own.Name)})
				return
			}
			mm.SetMocksWithWindow([]*models.Mock{a, b}, nil, w.Start, w.End)
		}
		ws := placeWindows()
		open("test-1", ws[0], a)
		if got := names(mm.OpenTestRecordings()); len(got) != 1 || got[0] != "mock-a" {
			t.Fatalf("test-1: %v, want [mock-a]", got)
		}
		per, err := mm.GetPerTestMocksInWindow()
		if err != nil || len(per) != 1 {
			t.Fatalf("precondition: test-1's pool is %v (%v)", names(per), err)
		}
		if !mm.DeleteFilteredMock(*per[0]) {
			t.Fatal("precondition: a call takes mock-a")
		}
		if got := names(mm.OpenTestRecordings()); len(got) != 1 || got[0] != "mock-a" {
			t.Fatalf("test-1 once a call took mock-a: %v, want [mock-a]", got)
		}
		if proven {
			// Between tests the pool's view is in force: no test is open,
			// and nothing staged a window. The next test's call in flight
			// it serves (ahead) is no open test's: none waits on it.
			mm.ServeExactPool("k", []string{"mock-b"}, roundMock(mm, "mock-b"))
			if got := names(mm.OpenTestRecordings()); len(got) != 0 {
				t.Fatalf("between tests: %v, want none", got)
			}
		}
		open("test-2", ws[1], b)
		if got := names(mm.OpenTestRecordings()); len(got) != 1 || got[0] != "mock-b" {
			t.Fatalf("test-2: %v, want [mock-b]", got)
		}
	})
}
