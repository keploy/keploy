package proxy

import (
	"net/netip"
	"slices"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// TestACutEndOwnsOnlyTheBytesWrittenBeforeIt pins who owns a lost range of
// the bytes the application wrote to a connection (pairedConn.lostBy): each
// end cut short owns the bytes written after the end before it and up to its
// own want, the test open at the count owns the rest, an end whose want is no
// more than the one before it owns none, and an end that names no test marks
// nothing.
func TestACutEndOwnsOnlyTheBytesWrittenBeforeIt(t *testing.T) {
	type cut struct {
		test string
		want uint64
	}
	for _, tc := range []struct {
		name     string
		cuts     []cut
		low, top uint64
		owners   []string
		open     bool
	}{
		{"no end cut short", nil, 3, 12, nil, true},
		{"an end that names no test", []cut{{"", 10}}, 3, 8, nil, true},
		{"all before the cut end", []cut{{"A", 10}}, 3, 8, []string{"A"}, false},
		{"the last byte is the cut end's want", []cut{{"A", 10}}, 10, 10, []string{"A"}, false},
		{"some written after the cut end", []cut{{"A", 10}}, 3, 12, []string{"A"}, true},
		{"all written after the cut end", []cut{{"A", 10}}, 11, 12, nil, true},
		{"all before the first of two cut ends", []cut{{"A", 5}, {"B", 10}}, 2, 4, []string{"A"}, false},
		{"all between two cut ends", []cut{{"A", 5}, {"B", 10}}, 6, 9, []string{"B"}, false},
		{"across two cut ends and after", []cut{{"A", 5}, {"B", 10}}, 4, 12, []string{"A", "B"}, true},
		{"ending at the first of two cut ends' wants", []cut{{"A", 5}, {"B", 10}}, 2, 5, []string{"A"}, false},
		{"a cut end that wrote nothing more", []cut{{"A", 10}, {"B", 10}}, 3, 12, []string{"A"}, true},
		{"a range from 0 begins at the first byte", []cut{{"A", 10}}, 0, 0, []string{"A"}, false},
		{"a range from 0 with no end cut short", nil, 0, 0, nil, true},
		{"a top under the low is the low", []cut{{"A", 10}}, 11, 5, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := &pairedConn{}
			for _, c := range tc.cuts {
				pc.cutShort(c.test, c.want)
			}
			owners, open := pc.lostBy(tc.low, tc.top)
			if !slices.Equal(owners, tc.owners) || open != tc.open {
				t.Fatalf("lostBy(%d, %d) = %q, %v; want %q, %v", tc.low, tc.top, owners, open, tc.owners, tc.open)
			}
		})
	}
}

// TestOwnedLossesPastTheBoundCountAsTheRuns pins the bound on the owned
// losses the proxy keeps while no agent takes them (pairedConns.own): past
// maxOwned, one entry of no test's counts every loss after, and a take empties
// them.
func TestOwnedLossesPastTheBoundCountAsTheRuns(t *testing.T) {
	var s pairedConns
	app := netip.MustParseAddrPort("127.0.0.1:50912")
	const past = 3
	for i := range maxOwned + past {
		s.own("test-"+strconv.Itoa(i+1), app)
	}
	if len(s.owned) != maxOwned+1 {
		t.Fatalf("%d owned losses kept, want %d", len(s.owned), maxOwned+1)
	}
	for i, o := range s.owned[:maxOwned] {
		if want := "test-" + strconv.Itoa(i+1); o.Test != want || o.Loss.Count != 1 || o.Loss.Kind != models.CaptureLossAppBytesLost {
			t.Fatalf("owned loss %d = %+v, want one app_bytes_lost of %s", i, o, want)
		}
	}
	if run := s.owned[maxOwned]; run.Test != "" || run.Loss.Count != past || run.Loss.Kind != models.CaptureLossAppBytesLost || run.Loss.What != agent.OwnedLossesPastTheBound {
		t.Fatalf("the run's owned loss = %+v, want %d app_bytes_lost of no test's", run, past)
	}
	p := &Proxy{}
	p.paired.owned = s.owned
	if got := len(p.TakeOwnedLosses()); got != maxOwned+1 {
		t.Fatalf("took %d owned losses, want %d", got, maxOwned+1)
	}
	if got := p.TakeOwnedLosses(); got != nil {
		t.Fatalf("a second take = %+v, want none", got)
	}
}

// TestAGapsOwnedLossSaysBetweenTestCases pins the words of an owned loss:
// a test's bytes were written in it, the gap's between test cases, never in
// a test case named for the gap.
func TestAGapsOwnedLossSaysBetweenTestCases(t *testing.T) {
	var s pairedConns
	app := netip.MustParseAddrPort("127.0.0.1:50912")
	s.own("test-1", app)
	s.own(agent.BetweenTestCases, app)
	want := []string{
		"from 127.0.0.1:50912, written in test-1, where keploy's wait for them was cut short",
		"from 127.0.0.1:50912, written between test cases, where keploy's wait for them was cut short",
	}
	if len(s.owned) != len(want) {
		t.Fatalf("owned %d losses, want %d", len(s.owned), len(want))
	}
	for i, o := range s.owned {
		if o.Loss.What != want[i] {
			t.Errorf("loss %d says %q, want %q", i, o.Loss.What, want[i])
		}
	}
}
