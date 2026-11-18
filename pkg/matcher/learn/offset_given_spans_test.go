package learn

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countGivenSpans counts the reads of instants (instantSpans) of texts that
// hold mark, until the row ends.
func countGivenSpans(t *testing.T, mark string) *atomic.Int64 {
	spans := instantSpans
	t.Cleanup(func() { instantSpans = spans })
	var read atomic.Int64
	instantSpans = func(s string) []instant {
		if strings.Contains(s, mark) {
			read.Add(1)
		}
		return spans(s)
	}
	return &read
}

// Every call of a test reads the test case's own request as keploy test sent
// it (offsetPlacesBy: the instants of each part that changed, recorded and
// sent), and each call read their instants anew: a test of n calls read them
// n times. They are read once for the test now (spansOf), however many calls
// read them, and each call pairs the same instants as the first. Counted, not
// timed.
func TestEachCallOfATestReadsItsRequestsChangedInstantsFromTheTestsReading(t *testing.T) {
	const mark = "given-spans-row"
	recAt, sentAt := eRecAt.Add(-time.Hour), eRecAt.Add(time.Hour)
	part := func(path string, at time.Time) Seen {
		return Seen{Side: Request, Leaf: Leaf{Path: path, Type: String, Value: mark + " since " + recAt.Format(time.RFC3339Nano) + " " + path}, Live: mark + " since " + at.Format(time.RFC3339Nano) + " " + path}
	}
	request := Earlier{Mock: "test-4", Given: true, Seen: []Seen{part("body.since", sentAt), part("query.from", sentAt.Add(time.Minute))}}
	for _, calls := range []int{4, 64} {
		read := countGivenSpans(t, mark)
		tr := NewTracker()
		tr.Begin()
		dc := tr.Drifts()
		first := dc.offsetPlacesOf(eRecAt, []Earlier{request}, nil, nil)
		// Each changed part paired, its recorded instant with the one sent
		// (once for each form the part is read in: as written and its other
		// declared form, rawExtra).
		sent := map[string]time.Time{"test-4 at body.since": sentAt, "test-4 at query.from": sentAt.Add(time.Minute)}
		paired := map[string]bool{}
		for _, p := range first.given {
			want, ok := sent[p[1].where]
			if !ok || p[0].where != p[1].where || !time.Unix(0, p[0].m.key.at).Equal(recAt) || !time.Unix(0, p[1].m.key.at).Equal(want) {
				t.Fatalf("%d calls: the first call paired %v", calls, p)
			}
			paired[p[1].where] = true
		}
		if len(paired) != len(sent) {
			t.Fatalf("%d calls: the first call paired the instants of %d of the request's %d changed parts (%v)", calls, len(paired), len(sent), first.given)
		}
		once := read.Load()
		if once == 0 {
			t.Fatalf("%d calls: the first call read none of the request's instants", calls)
		}
		for i := 1; i < calls; i++ {
			op := dc.offsetPlacesOf(eRecAt, []Earlier{request}, nil, nil)
			if fmt.Sprint(op.given) != fmt.Sprint(first.given) {
				t.Fatalf("%d calls: call %d paired\n%v\nwhere the first paired\n%v", calls, i+1, op.given, first.given)
			}
		}
		tr.End()
		if got := read.Load(); got != once {
			t.Errorf("a test of %d calls read its request's changed instants %d times, the first call %d: want the first call's reads alone (once for the test)", calls, got, once)
		}
	}
}

// A test's reading of its request's instants is its own: the next test,
// sending the same parts at other instants, pairs its own instants, read
// anew, never the last test's.
func TestTheNextTestPairsItsOwnRequestsInstants(t *testing.T) {
	const mark = "given-spans-next"
	recAt := eRecAt.Add(-time.Hour)
	request := func(at time.Time) Earlier {
		return Earlier{Mock: "test-4", Given: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.since", Type: String, Value: mark + " " + recAt.Format(time.RFC3339Nano)}, Live: mark + " " + at.Format(time.RFC3339Nano)}}}
	}
	tr := NewTracker()
	var got []string
	for _, at := range []time.Time{eRecAt.Add(time.Hour), eRecAt.Add(2 * time.Hour)} {
		tr.Begin()
		op := tr.Drifts().offsetPlacesOf(eRecAt, []Earlier{request(at)}, nil, nil)
		tr.End()
		if len(op.given) == 0 {
			t.Fatalf("a request sent at %v paired none of its instants", at)
		}
		for _, p := range op.given {
			if live := time.Unix(0, p[1].m.key.at).UTC(); !live.Equal(at) {
				t.Fatalf("a request sent at %v paired its part with the instant %v", at, live)
			}
		}
		got = append(got, fmt.Sprint(op.given))
	}
	if got[0] == got[1] {
		t.Fatalf("two tests sending their request at other instants paired the same: %v", got[0])
	}
}
