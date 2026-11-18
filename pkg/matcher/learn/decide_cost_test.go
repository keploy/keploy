package learn

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// decideAllocs decides costSet's call over n recordings of the set, in the
// set's second test (the first built what the set keeps), and returns what
// it decided and its allocations per Decide. shape says whose the recordings
// are: "own" the call's test's, "other" other tests' (Raw.OfAnotherTest),
// "half" half each, its own first.
func decideAllocs(t *testing.T, shape string, n int) (string, float64) {
	t.Helper()
	const rec, live = "ids a1b2c3d4 b2c3d4e5 c3d4e5f6 d4e5f6a7 e5f6a7b8", "ids f0e9d8c7 e9d8c7b6 d8c7b6a5 c7b6a5f4 b6a5f4e3"
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		name, body, at := fmt.Sprintf("mock-%d", i), fmt.Sprintf(`{"note":"Item %d of the run","ref":"Ref-%05d"}`, i, i), eRecAt.Add(-time.Duration(n-i)*time.Millisecond)
		if i == n/2 {
			name, body, at = "mock-held", `{"note":"Kept A1B2C3D4","ref":"a1b2c3d4"}`, eRecAt.Add(-time.Second)
		}
		mocks[i] = getAnswer(name, fmt.Sprintf("http://api/items/%d", i), body, at)
	}
	tr := NewTracker()
	tr.NewSet()
	input := func() Input {
		dc := tr.Drifts()
		raw := lazyOf(dc, mocks)
		for i := range raw {
			if shape == "other" || shape == "half" && i < n/2 {
				raw[i] = raw[i].OfAnotherTest()
			}
		}
		if shape == "half" {
			raw = append(raw[n/2:n:n], raw[:n/2]...)
		}
		return costCall(dc, "body.ids", rec, live, eRecAt, nil, raw, nil, nil)
	}
	tr.Begin()
	Decide(input())
	tr.End()
	tr.Begin()
	defer tr.End()
	in := input()
	got := oDescribe(Decide(in))
	return got, testing.AllocsPerRun(20, func() { Decide(in) })
}

// A call's decision allocates alike over 300 and 3,000 earlier recordings
// of its set, of its own test, of other tests or of both: its handles of
// the places' texts, its postings of their words and lists, its recordings'
// offset keys and its outside recordings' words each take a block per call
// or what the set keeps, never an allocation per recording. Each was one or
// two per recording per call, the most of a call's work over api-server's
// set besides the reads themselves. The decisions stay.
func TestADecisionAllocatesAlikeOverAnySetSize(t *testing.T) {
	for _, shape := range []string{"own", "other", "half"} {
		few, fewAllocs := decideAllocs(t, shape, 300)
		many, manyAllocs := decideAllocs(t, shape, 3000)
		t.Logf("%s recordings: %v allocations per decision over 300, %v over 3,000; decided %s", shape, fewAllocs, manyAllocs, many)
		if many != few {
			t.Errorf("over %s recordings the call decides %q over 3,000 and %q over 300; want one verdict", shape, many, few)
		}
		if manyAllocs > fewAllocs {
			t.Errorf("over %s recordings a decision allocates %v times over 3,000 and %v over 300; want no more over 3,000", shape, manyAllocs, fewAllocs)
		}
	}
}

// postings read each id's adds, whatever order they were added in, and none
// for an id no add has, at either end or between.
func TestPostingsReadEachIDsAddsAndNoneOfAnother(t *testing.T) {
	ps := newPostings(0)
	adds := []posting{{7, 0}, {3, 1}, {7, 2}, {5, 3}, {3, 4}, {9, 5}, {7, 6}}
	for _, p := range adds {
		ps.add(p.id, p.i)
	}
	ps.seal()
	for _, id := range []int32{1, 3, 4, 5, 6, 7, 8, 9, 10} {
		var want, got []int32
		for _, p := range adds {
			if p.id == id {
				want = append(want, p.i)
			}
		}
		for _, p := range ps.of(id) {
			if p.id != id {
				t.Errorf("id %d reads a posting of id %d", id, p.id)
			}
			got = append(got, p.i)
		}
		if !sameInts(got, want) {
			t.Errorf("id %d reads %v; want %v, in any order", id, got, want)
		}
	}
}

// sameInts reports whether a and b hold the same values as often.
func sameInts(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	n := map[int32]int{}
	for _, v := range a {
		n[v]++
	}
	for _, v := range b {
		n[v]--
	}
	for _, c := range n {
		if c != 0 {
			return false
		}
	}
	return true
}

// A recording's offset key is its mock, a zero byte and its instant as
// String writes it, the monotonic reading of one read from the clock
// included, and a key written into a block with room allocates nothing for
// an instant with no monotonic reading, as a recording's is.
func TestARecordingsOffsetKeyIsItsInstantAsStringWritesIt(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	ats := []time.Time{
		{},
		eRecAt,
		eRecAt.Add(123456789 * time.Nanosecond).In(ist),
		eRecAt.In(time.FixedZone("", -7*3600)),
		time.Now(),
		time.Now().Round(0),
	}
	for _, at := range ats {
		if got, want := string(appendOffsetKey([]byte("x"), "mock-7", at)), "xmock-7\x00"+at.String(); got != want {
			t.Errorf("the key of %v is %q; want %q", at, got, want)
		}
	}
	b := make([]byte, 0, offsetKeyRoom)
	if n := testing.AllocsPerRun(10, func() { b = appendOffsetKey(b[:0], "mock-7", eRecAt.In(ist)) }); n != 0 {
		t.Errorf("a key written into a block with room allocates %v times; want none", n)
	}
	keys := newOffsetKeys(1)
	first := keys.of("mock-1", eRecAt)
	for i := range 20 {
		keys.of(fmt.Sprintf("mock-%d", i), eRecAt.Add(time.Duration(i)))
	}
	if want := offsetKeyOf("mock-1", eRecAt); string(first) != want {
		t.Errorf("a key reads %q once the block grew past it; want %q", first, want)
	}
}

// The set keeps a recording's words with its name and connection, its own
// copies, counted on the set's budget, and a call handed the recording
// again reads them with no copy of either; a recording handed with
// another name or connection than the entry's gets its own, as a copy.
func TestASetsKeptWordsNameTheRecordingTheCallHands(t *testing.T) {
	name, conn := strings.Repeat("mock-a", 8), strings.Repeat("conn-a", 8)
	g := givenWords{mock: name, conn: conn}
	e := keptWordsOf(wordsKey{}, g)
	if bare := keptWordsOf(wordsKey{}, givenWords{}); e.bytes <= bare.bytes {
		t.Errorf("an entry kept with a %d-letter name and connection counts %d bytes, one with neither %d; want the copies counted", len(name), e.bytes, bare.bytes)
	}
	x := Raw{mock: strings.Clone(name), conn: strings.Clone(conn)}
	if got := e.of(x); got.mock != name || got.conn != conn {
		t.Errorf("the entry reads the recording as %q on %q; want %q on %q", got.mock, got.conn, name, conn)
	}
	if n := testing.AllocsPerRun(10, func() { e.of(x) }); n != 0 {
		t.Errorf("the entry reads the recording it keeps with %v allocations; want none", n)
	}
	other := Raw{mock: "mock-b", conn: "conn-b"}
	if got := e.of(other); got.mock != "mock-b" || got.conn != "conn-b" {
		t.Errorf("the entry reads a recording named %q on %q as %q on %q; want its own", other.mock, other.conn, got.mock, got.conn)
	}
}
