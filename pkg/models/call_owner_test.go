package models

import (
	"testing"
	"time"
)

// CallOwner names a call's owner only by the process tree: a worker on the
// chain, then the innermost of that one worker's tests; else the only test.
func TestCallOwnerTellsTheOwnerOnlyByTheProcessTree(t *testing.T) {
	t0 := time.Unix(7000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	for _, tc := range []struct {
		name  string
		cands []OwnerCandidate
		chain []uint32
		want  int
	}{
		{"no candidate", nil, []uint32{1}, -1},
		{"the only candidate, no chain", []OwnerCandidate{{100, at(0)}}, nil, 0},
		{"the only candidate, the process not its worker's", []OwnerCandidate{{100, at(0)}}, []uint32{300}, 0},
		{"the worker's child", []OwnerCandidate{{100, at(0)}, {200, at(1)}}, []uint32{201, 200, 1}, 1},
		{"the worker itself, listed first", []OwnerCandidate{{200, at(1)}, {100, at(0)}}, []uint32{200, 1}, 0},
		{"neither worker", []OwnerCandidate{{100, at(0)}, {200, at(1)}}, []uint32{300, 1}, -1},
		{"no chain, two tests", []OwnerCandidate{{100, at(0)}, {200, at(1)}}, nil, -1},
		{"tests with no worker", []OwnerCandidate{{0, at(0)}, {0, at(1)}}, []uint32{0, 300}, -1},
		{"one worker's nested tests: the latest begun", []OwnerCandidate{{100, at(2)}, {100, at(0)}, {100, at(1)}}, []uint32{100}, 0},
		{"one worker's nested tests, the process not of it", []OwnerCandidate{{100, at(0)}, {100, at(1)}}, []uint32{300}, -1},
		{"one worker's innermost two begun at one instant", []OwnerCandidate{{100, at(0)}, {100, at(1)}, {100, at(1)}}, []uint32{100}, -1},
		{"an earlier tie under a later innermost", []OwnerCandidate{{100, at(0)}, {100, at(0)}, {100, at(1)}}, []uint32{100}, 2},
		{"two workers both on the chain", []OwnerCandidate{{100, at(0)}, {200, at(1)}}, []uint32{201, 200, 100, 1}, -1},
		{"a worker on the chain beside one that is not", []OwnerCandidate{{100, at(0)}, {200, at(0)}, {0, at(0)}}, []uint32{100}, 0},
	} {
		if got := CallOwner(tc.cands, tc.chain); got != tc.want {
			t.Errorf("%s: CallOwner = %d, want %d", tc.name, got, tc.want)
		}
	}
}
