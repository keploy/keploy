package learn

import (
	"fmt"
	"testing"
)

// tallyOf is how many times each part of the set's recordings was read
// (derive), by tier and part.
func tallyOf(s *setDerived) map[readTally]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[readTally]int, len(s.reads))
	for k, v := range s.reads {
		out[k] = v
	}
	return out
}

// readSince is the parts read between two tallies (tallyOf), each with how
// many times.
func readSince(before, after map[readTally]int) map[readTally]int {
	out := map[readTally]int{}
	for k, v := range after {
		if d := v - before[k]; d != 0 {
			out[k] = d
		}
	}
	return out
}

// A staged set's calls (WarmSet) read no part of any recording on their
// answer path but where their drift's words stand: no set-wide work, the
// first drifted call's neither. Before, the staging read three parts of
// each recording and the set's first drifted call read four more of every
// recording before it, whatever it drifted with: its keyed places and its
// answer's stamps, and, where an earlier place echoed what F saw, its
// fields' instants twice over (api-server's round: 15 589 recordings, read
// while the call's client waited). A word of the drift that stands in one
// recording is looked for there only: its words are read for that one, its
// filter tells the rest hold none. Counted by part, not timed; each test
// learns what the set with no staging learns.
func TestASetsCallsAfterItsStagingReadOnlyWhatTheirDriftTouches(t *testing.T) {
	const n, tests, touched = 48, 4, 17
	tokens := map[readTally]bool{{tierWords, byKeep(partTokens, false)}: true, {tierWords, byKeep(partTokens, true)}: true}
	for _, c := range []struct {
		name       string
		echo, word bool
	}{{"no echo, no word", false, false}, {"an earlier place echoes F's moment", true, false}, {"an earlier recording holds a changed word", false, true}, {"both", true, true}} {
		t.Run(c.name, func(t *testing.T) {
			row := newHeldRow(n, 4)
			if c.echo {
				// What F saw of the drifted clock read, held by an earlier
				// recording: the echo reads every recording's fields.
				row.mocks[n/2].Spec.PostgresV3.Query.Response.Rows[0][1].Value = "2026-10-24T09:56:54.1Z"
			}
			if c.word {
				cell := &row.mocks[touched].Spec.PostgresV3.Query.Response.Rows[0][0]
				cell.Value = fmt.Sprint(cell.Value) + " z9y8x7w6"
			}
			raw := row.raw()
			for i, m := range row.mocks {
				raw[i] = RawOf(m, true).ServedAs(i + 1).ServedAsRecorded()
			}
			plain := NewTracker()
			plain.NewSet()
			plain.Begin()
			want := row.classify(plain.Drifts())
			plain.End()

			tr := NewTracker()
			tr.NewSet()
			if use := <-tr.WarmSet(srcOf(row.mocks)); use.Staged != n || use.HeldFull || use.FormsFull {
				t.Fatalf("the set's staging reported %+v: want all %d read by it, within its budget", use, n)
			}
			s := tr.Drifts().set
			before := tallyOf(s)
			for i := 0; i < tests; i++ {
				tr.Begin()
				got := row.classify(tr.Drifts())
				tr.End()
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("test %d of the staged set learned\n%v\nwhere the set with no staging learned\n%v", i+1, got, want)
				}
				after := tallyOf(s)
				read := readSince(before, after)
				before = after
				words := 0
				for k, d := range read {
					if !tokens[k] {
						t.Fatalf("test %d's call read part %#x (tier %d) of %d of the staged set's recordings: want none (all reads %v)", i+1, uint32(k.part), k.tier, d, read)
					}
					words += d
				}
				switch {
				case i > 0 && words != 0:
					t.Fatalf("test %d's call read the words of %d recordings: want none, the first test read them", i+1, words)
				case i == 0 && c.word && words == 0:
					t.Fatalf("test 1's call read no recording's words: want the one that holds a changed word (%d) read", touched)
				case words > len(tokens):
					t.Fatalf("test %d's call read the words of recordings %d times (all reads %v): want at most the one recording that holds a changed word, once for each way it is searched", i+1, words, read)
				}
			}
		})
	}
}
