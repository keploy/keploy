//go:build linux

package proxy

import (
	"slices"
	"testing"
)

// The different call is recorded, staged and served BEFORE the two
// identical ones, so the open test's recordings a held answer could be a
// twin of come in the order [the different one, the identical one]. A
// judgement of only the first twin then sees the different one and holds the
// answer the test waits on; the head must judge each.
func TestHeldAnswer_ATwinRecordedAfterAnotherCallIsStillJudged(t *testing.T) {
	for _, bgFirst := range []bool{true, false} {
		bothSwitches(t, func(t *testing.T, proven bool) {
			bgReq, fgReq := placeAt(3), placeAt(2)
			if bgFirst {
				bgReq, fgReq = placeAt(2), placeAt(3)
			}
			s := stagePlacedSet(t, proven, placeWindows(),
				placedMock("mock-other", "/other", placeAt(1), placeAt(1)),
				placedMock("mock-bg", "/same", bgReq, placeAt(45)),
				placedMock("mock-fg", "/same", fgReq, placeAt(5)))
			s.open(t, "test-1", "mock-other", "mock-bg", "mock-fg")
			placedWritten(t, s.call(t, "/other"), "mock-other", "the other call")
			a := placedAnswered(t, s.call(t, "/same"), "the first identical call")
			b := placedAnswered(t, s.call(t, "/same"), "the second identical call")
			got := []string{a, b}
			slices.Sort(got)
			if !slices.Equal(got, []string{"mock-bg", "mock-fg"}) {
				t.Fatalf("the two calls read %q, want each recording once", got)
			}
			s.end(t, "test-1")
		})
	}
}
