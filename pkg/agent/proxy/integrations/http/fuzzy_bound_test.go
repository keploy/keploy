package http

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

// fuzzyBodies is a request body of n lowercase letters and two recordings
// of it that the two rankings order apart: near has every fourth letter
// changed (a Levenshtein distance of n/4, and no 8-letter shingle in common),
// rotated is the body's halves swapped (a distance of about n, and every
// shingle but those across the cut in common). Levenshtein picks near,
// Jaccard rotated.
func fuzzyBodies(n int) (req, near, rotated string) {
	req = fuzzyLetters(n, 2463534242)
	c := []byte(req)
	for i := 0; i < n; i += 4 {
		c[i] = 'a' + (c[i]-'a'+1)%26
	}
	return req, string(c), req[n/2:] + req[:n/2]
}

// fuzzyLetters is n pseudo-random lowercase letters drawn from seed.
func fuzzyLetters(n int, seed uint32) string {
	b := make([]byte, n)
	x := seed
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = 'a' + byte(x%26)
	}
	return string(b)
}

func fuzzyMock(name, body string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq: &models.HTTPReq{Method: models.Method("POST"), URL: "http://up.example/poll", Body: body},
	}}
}

// A request is ranked against its candidates by Levenshtein distance only
// while the distances fit their bound; past it, by the similarity of their
// shingles, in time linear in the bodies. Unbounded, the 200 KB row (a
// poller's body against its recordings) took about a minute of CPU per
// candidate.
func TestPerformFuzzyMatchBoundsLevenshtein(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	for _, tc := range []struct {
		n    int
		want string
	}{
		{n: 256, want: "near"},       // 256*256*2 steps: Levenshtein
		{n: 4096, want: "rotated"},   // 4096*4096*2 steps, over the bound: shingles
		{n: 200000, want: "rotated"}, // past the 65535 characters a distance counts to
	} {
		req, near, rotated := fuzzyBodies(tc.n)
		ok, got := h.PerformFuzzyMatch([]*models.Mock{fuzzyMock("near", near), fuzzyMock("rotated", rotated)}, []byte(req))
		if !ok || got == nil {
			t.Fatalf("n=%d: no candidate picked", tc.n)
		}
		if got.Name != tc.want {
			t.Errorf("n=%d: picked %s, want %s", tc.n, got.Name, tc.want)
		}
	}
}

// No part over 65535 characters is ranked by Levenshtein distance, even when
// the steps fit their bound: the library counts a distance in 16 bits, and a
// true distance of 65536 comes out as 0, an exact match. Each row's wrapped
// recording is that far from the request: past their common end, one
// character none of the other's holds against 65536. Levenshtein would pick
// it; the other recording is far closer by their shingles, which pick it.
func TestPerformFuzzyMatchKeepsWrappedDistancesOut(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	pad := fuzzyLetters(65536, 88172645)
	small := fuzzyLetters(200, 2463534242)
	oneOff := []byte(small)
	oneOff[100] = 'a' + (oneOff[100]-'a'+1)%26
	for _, tc := range []struct {
		name  string
		req   string
		mocks []*models.Mock
	}{
		// 1 * 65536 steps past the common end; the recording's part is too long.
		{"a short request against a recording over 65535 characters", "0" + small,
			[]*models.Mock{fuzzyMock("wrapped", pad+small), fuzzyMock("shingles", "1"+string(oneOff))}},
		// 65536 * 1 steps past the common end; the request's part is too long.
		{"a request over 65535 characters against short recordings", pad + small[:10],
			[]*models.Mock{fuzzyMock("wrapped", "0"+small[:10]), fuzzyMock("shingles", pad[1000:1245])}},
	} {
		var cells int64
		for _, m := range tc.mocks {
			a, b := trimCommonEnds(tc.req, m.Spec.HTTPReq.Body)
			cells += int64(len(a)) * int64(len(b))
		}
		if m := tc.mocks[0]; cells > levenshteinMaxCells {
			t.Fatalf("%s: %d steps with %s, over the step bound: the row would not test the length bound alone", tc.name, cells, m.Name)
		}
		ok, got := h.PerformFuzzyMatch(tc.mocks, []byte(tc.req))
		if !ok || got == nil {
			t.Fatalf("%s: no candidate picked", tc.name)
		}
		if got.Name != "shingles" {
			t.Errorf("%s: picked %s, want shingles (a wrapped distance read as a match)", tc.name, got.Name)
		}
	}
}

// fuzzyPost is a request as decodeHTTP reads it, with body.
func fuzzyPost(body string) string {
	return "POST /poll HTTP/1.1\r\nHost: api.example\r\nContent-Type: application/json\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

var jsonHeader = map[string]string{"Content-Type": "application/json"}

func jsonQ(s string) string { return `{"q":"` + s + `"}` }

// heavyDB is a store of n recordings of /poll of size bytes each: a call to
// it weighs over heavyCallBytes.
func heavyDB(t *testing.T, n, size int) *mockMemDb {
	t.Helper()
	if int64(n)*int64(size) <= heavyCallBytes {
		t.Fatalf("%d recordings of %d bytes are no heavy call", n, size)
	}
	db := &mockMemDb{updateUnFilteredReturn: true}
	for i := 0; i < n; i++ {
		db.mocks = append(db.mocks, recorded(fmt.Sprintf("m%d", i), "/poll", jsonHeader, jsonQ(fuzzyLetters(size, uint32(1000+i*7919)))))
	}
	return db
}

// takeTurns holds n heavy turns until the function it returns is called.
func takeTurns(t *testing.T, n int) (giveBack func()) {
	t.Helper()
	turns := heavyTurns()
	for i := 0; i < n; i++ {
		select {
		case turns <- struct{}{}:
		case <-time.After(5 * time.Second):
			t.Fatal("a heavy turn was still held by another call")
		}
	}
	given := false
	giveBack = func() {
		if !given {
			given = true
			for i := 0; i < n; i++ {
				<-turns
			}
		}
	}
	t.Cleanup(giveBack)
	return giveBack
}

// takeAllTurns holds every heavy turn until the function it returns is called.
func takeAllTurns(t *testing.T) (giveBack func()) {
	t.Helper()
	return takeTurns(t, cap(heavyTurns()))
}

// settled waits for the goroutines to come back to base, and for the heap to
// come within slack of heapBase, and fails naming which did not. A byte held
// past a call stays in the heap across every collection.
func settled(t *testing.T, what string, base int, heapBase, slack uint64) {
	t.Helper()
	var ms runtime.MemStats
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		runtime.GC()
		runtime.ReadMemStats(&ms)
		if runtime.NumGoroutine() <= base && ms.HeapAlloc <= heapBase+slack {
			return
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("%s: %d goroutines (%d before), %d MB of heap (%d MB before) 5 s after",
				what, runtime.NumGoroutine(), base, ms.HeapAlloc>>20, heapBase>>20)
		}
	}
}

func heapNow() uint64 {
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// liveHeap is the heap a collection forced here found live. HeapAlloc read
// after it also counts what other goroutines allocated since, which on a
// loaded machine can be several of a ranking's recordings.
func liveHeap() uint64 {
	runtime.GC()
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// onRanking is an HTTP that calls begun, once, on the ranking's own goroutine
// where a ranking begins, and counts the rankings that ran to a pick: those
// two lines of its log are all a ranking shows from outside.
func onRanking(begun func()) (h *HTTP, picks *atomic.Int64) {
	picks = &atomic.Int64{}
	var once sync.Once
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zapcore.EncoderConfig{}), zapcore.AddSync(io.Discard), zap.DebugLevel)
	h = newHTTP()
	h.Logger = zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		switch e.Message {
		case "mocks under consideration for performfuzzyMatch function":
			once.Do(begun)
		case "http mock matched":
			picks.Add(1)
		}
		return nil
	}))
	return h, picks
}

// Calls that come faster than one can be ranked do not pile up: four
// identical 200 KB JSON posts at once, to a path with two recordings of that
// size, are each answered within the 5 s after which a client gives up and
// calls again beside them, and once they are no goroutine of theirs is left.
func TestConcurrentLargeBodiesAreAnsweredAndReleased(t *testing.T) {
	r, near, rotated := fuzzyBodies(200000)
	base := runtime.NumGoroutine()
	const calls = 4
	picked := make(chan string, calls)
	for i := 0; i < calls; i++ {
		db := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
			recorded("near", "/poll", jsonHeader, jsonQ(near)), recorded("rotated", "/poll", jsonHeader, jsonQ(rotated))}}
		go func() {
			in := sent("/poll", jsonHeader, jsonQ(r))
			in.raw = in.body
			ok, m, _, err := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, false, false)
			switch {
			case err != nil:
				picked <- "error: " + err.Error()
			case !ok || m == nil:
				picked <- "no recording"
			default:
				picked <- m.Name
			}
		}()
	}
	deadline := time.After(5 * time.Second)
	for i := 0; i < calls; i++ {
		select {
		case got := <-picked:
			if got != "rotated" {
				t.Errorf("call answered with %q, want rotated (ranked by shingles past the bound)", got)
			}
		case <-deadline:
			t.Fatalf("%d of %d concurrent 200 KB calls unanswered after 5 s: their client has given up on them", calls-i, calls)
		}
	}
	for start := time.Now(); runtime.NumGoroutine() > base; time.Sleep(10 * time.Millisecond) {
		if time.Since(start) > 5*time.Second {
			t.Fatalf("%d goroutines 5 s after every call was answered, %d before the calls", runtime.NumGoroutine(), base)
		}
	}
}

// One 200 KB recording is no cheaper to rank by distance than two: a call
// with a single candidate is held to the bound too.
func TestOneLargeCandidateIsBounded(t *testing.T) {
	a, b := fuzzyLetters(200000, 7), fuzzyLetters(200000, 11)
	start := time.Now()
	if ok, got := newHTTP().PerformFuzzyMatch([]*models.Mock{fuzzyMock("only", b)}, []byte(a)); !ok || got.Name != "only" {
		t.Fatalf("got %v, %v: want the one candidate", ok, got)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("one 200 KB candidate took %v to rank", took)
	}
}

// Recordings that differ only in how many rows they hold, from a call whose
// timestamp drifted, are told apart by the rows at any size, wherever the
// right one is listed: at the rows' length a set of shingles sees them all
// alike. The timestamp is in one place (a Levenshtein distance of the rows
// that differ, once the common ends are trimmed) or in every row (over the
// bound past 120 rows: the shingles' counts tell them apart).
func TestDriftingRowsAreToldApartByTheirCount(t *testing.T) {
	for _, shape := range []struct {
		name string
		row  func(i int, ts string) string
	}{
		{"the same row repeated", func(int, string) string { return `{"sku":"A1","qty":1,"state":"open"},` }},
		{"few distinct rows", func(i int, _ string) string { return fmt.Sprintf(`{"sku":"A%d","qty":%d,"state":"open"},`, i%4, i%3) }},
		{"distinct ids", func(i int, _ string) string { return fmt.Sprintf(`{"id":%d,"qty":1,"state":"open"},`, 100000+i) }},
		{"the same row with its own timestamp", func(_ int, ts string) string { return `{"sku":"A1","at":"` + ts + `"},` }},
		{"few distinct rows with their own timestamp", func(i int, ts string) string { return fmt.Sprintf(`{"sku":"A%d","at":"%s"},`, i%4, ts) }},
	} {
		body := func(ts string, rows int) string {
			var b strings.Builder
			b.WriteString(`{"ts":"` + ts + `","items":[`)
			for i := 0; i < rows; i++ {
				b.WriteString(shape.row(i, ts))
			}
			b.WriteString(`{"end":true}]}`)
			return b.String()
		}
		for _, rows := range []int{120, 200, 500, 1500} {
			for right := 0; right < 3; right++ {
				others := []int{rows + 20, rows - 10}
				var mocks []*models.Mock
				for i := 0; i < 3; i++ {
					n, name := rows, "right"
					if i != right {
						n, others = others[0], others[1:]
						name = fmt.Sprintf("%d rows", n)
					}
					mocks = append(mocks, recorded(name, "/batch", jsonHeader, body(fmt.Sprintf("2026-11-06T0%d:11:22.123Z", i+1), n)))
				}
				live := body("2026-11-06T09:44:55.987Z", rows)
				in := sent("/batch", jsonHeader, live)
				in.raw = []byte(fuzzyPost(live))
				db := &mockMemDb{updateUnFilteredReturn: true, mocks: mocks}
				ok, m, _, err := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, false, false)
				if err != nil || !ok || m == nil {
					t.Fatalf("%s, %d rows: ok=%v err=%v", shape.name, rows, ok, err)
				}
				if m.Name != "right" {
					t.Errorf("%s, %d rows, right listed at %d: picked %s", shape.name, rows, right, m.Name)
				}
			}
		}
	}
}

// A call is ranked by its body: what its headers weigh does not push it past
// the bound, as the raw request's length did, nor into a recording's score.
func TestTheBoundMeasuresTheBody(t *testing.T) {
	r, near, rotated := fuzzyBodies(256)
	in := sent("/poll", nil, r)
	in.raw = []byte("POST /poll HTTP/1.1\r\nX-Pad: " + strings.Repeat("h", 70000) + "\r\n\r\n" + r)
	db := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{recorded("rotated", "/poll", nil, rotated), recorded("near", "/poll", nil, near)}}
	ok, m, _, err := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, false, false)
	if err != nil || !ok || m == nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if m.Name != "near" {
		t.Errorf("picked %s, want near: the body, not its headers, is measured and ranked", m.Name)
	}
}

// What is picked past the two rankings' own scores: a recording equal to the
// request wherever it is listed, the first of those tied, shingles for a body
// Levenshtein does not rank, and nothing with no candidate.
func TestWhatARankingPicks(t *testing.T) {
	r, near, rotated := fuzzyBodies(256)
	accent := func(s string) string { return "\u00e9" + s }
	for _, tc := range []struct {
		name  string
		req   string
		mocks []*models.Mock
		want  string
	}{
		{"equal to the request, listed last", r, []*models.Mock{fuzzyMock("near", near), fuzzyMock("equal", r)}, "equal"},
		{"tied by distance", "abcdefgh", []*models.Mock{fuzzyMock("first", "abcdefgX"), fuzzyMock("second", "abcdefgY")}, "first"},
		{"tied by shingles", accent(r), []*models.Mock{fuzzyMock("first", r+"X"), fuzzyMock("second", r+"Y")}, "first"},
		{"a non-ASCII request goes by shingles", accent(r), []*models.Mock{fuzzyMock("near", near), fuzzyMock("rotated", rotated)}, "rotated"},
		{"non-ASCII recordings go by shingles", r, []*models.Mock{fuzzyMock("near", accent(near)), fuzzyMock("rotated", accent(rotated))}, "rotated"},
		{"a non-ASCII recording is left out of distances", r, []*models.Mock{fuzzyMock("accent", accent(r)), fuzzyMock("near", near)}, "near"},
	} {
		ok, got := newHTTP().PerformFuzzyMatch(tc.mocks, []byte(tc.req))
		if !ok || got == nil || got.Name != tc.want {
			t.Errorf("%s: picked %v, %v, want %s", tc.name, ok, got, tc.want)
		}
	}
	if ok, got := newHTTP().PerformFuzzyMatch(nil, []byte(r)); ok || got != nil {
		t.Errorf("no candidates: picked %v, %v", ok, got)
	}
}

// A recording equal to the request is picked past the bound too, where a
// sample of shingles can read a recording a byte away as equal to it.
func TestAnEqualRecordingIsPickedPastTheBound(t *testing.T) {
	req := fuzzyLetters(2_000_000, 13)
	bag := newShingleBag(req)
	if bag.sample == 0 {
		t.Fatalf("the bag of a %d-byte body kept every shingle", len(req))
	}
	var away string
	for i := 1000; away == "" && i < len(req); i += 1000 {
		b := []byte(req)
		b[i] = 'A'
		if bag.similarity(string(b)) == 1 {
			away = string(b)
		}
	}
	if away == "" {
		t.Fatal("every byte of the request changes a shingle the bag kept")
	}
	mocks := []*models.Mock{fuzzyMock("unrelated", fuzzyLetters(2_000_000, 17)), fuzzyMock("a byte away", away), fuzzyMock("equal", req)}
	if ok, got := newHTTP().PerformFuzzyMatch(mocks, []byte(req)); !ok || got == nil || got.Name != "equal" {
		t.Errorf("picked %v, %v, want equal", ok, got)
	}
}

// A recording's noisy values are out of its score: what the obfuscator padded
// does not push a recording of the very call away from it.
func TestNoisyValuesAreOutOfTheScore(t *testing.T) {
	padded := fuzzyMock("padded", `{"a":"xxxxxxxxxxxxxxxx","token":"`+strings.Repeat("R", 400)+`"}`)
	padded.Noise = []string{"^R+$"}
	near := fuzzyMock("near", `{"a":"xxxxxxxxxxxxxxxy"}`)
	if ok, got := newHTTP().PerformFuzzyMatch([]*models.Mock{near, padded}, []byte(`{"a":"xxxxxxxxxxxxxxxx"}`)); !ok || got == nil || got.Name != "padded" {
		t.Errorf("picked %v, %v, want padded", ok, got)
	}
}

// endsAfter is a context that ends after its n-th read of Err.
type endsAfter struct {
	context.Context
	n *atomic.Int64
}

func (c endsAfter) Err() error {
	if c.n.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

// A call cut short is no miss, wherever it is cut: no read of its end, up to
// the last the ranking makes, leaves it reported as a call no recording fits.
func TestAMatchCutShortIsNoMiss(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		mocks      []string
	}{
		{"by distance", jsonQ("aaab"), []string{jsonQ("aaaa"), jsonQ("bbbb")}},
		{"by shingles", jsonQ(fuzzyLetters(8000, 3)), []string{jsonQ(fuzzyLetters(8000, 5)), jsonQ(fuzzyLetters(8000, 7))}},
	} {
		served := false
		for n := int64(0); n < 200 && !served; n++ {
			left := &atomic.Int64{}
			left.Store(n)
			db := &mockMemDb{updateUnFilteredReturn: true}
			for i, b := range tc.mocks {
				db.mocks = append(db.mocks, recorded(fmt.Sprintf("m%d", i), "/poll", jsonHeader, b))
			}
			in := sent("/poll", jsonHeader, tc.body)
			in.raw = in.body
			ok, _, _, err := newHTTP().match(endsAfter{context.Background(), left}, in, db, nil, nil, nil, false, false, false, false, false)
			if !ok && err == nil {
				t.Fatalf("%s: a call ended at its %d-th read of its end reads as a miss", tc.name, n)
			}
			served = ok
		}
		if !served {
			t.Fatalf("%s: never served", tc.name)
		}
	}
}

// A shingle that repeats counts each time, up to the times the request holds
// it, and a recording's other shingles count against it.
func TestShingleBagCountsRepeats(t *testing.T) {
	row := "abcdefghij,"
	req := strings.Repeat(row, 100)
	bag := newShingleBag(req)
	same := bag.similarity(req)
	fewer := bag.similarity(strings.Repeat(row, 90))
	more := bag.similarity(strings.Repeat(row, 110))
	other := bag.similarity(strings.Repeat("0123456789;", 100))
	if same != 1 || fewer <= more-0.05 || more >= same || fewer >= same || other != 0 {
		t.Errorf("similarity: same %v, 90 rows %v, 110 rows %v, unrelated %v", same, fewer, more, other)
	}
	if again := bag.similarity(strings.Repeat(row, 90)); again != fewer {
		t.Errorf("a second reading of a recording scores %v, the first %v", again, fewer)
	}
	if got := newShingleBag("").similarity(""); got != 0 {
		t.Errorf("two empty bodies: %v, want 0", got)
	}
	if got := newShingleBag("ab").total; got != 0 {
		t.Errorf("a body shorter than a shingle holds %d, want none", got)
	}
	if got := newShingleBag("abcd").total; got != 2 {
		t.Errorf("4 bytes hold %d shingles of 3, want 2", got)
	}
}

// What a ranking holds of its request does not grow with the body: past
// maxBagShingles distinct shingles it keeps a sample, the same for the
// recordings, which still scores a recording by what it shares with the call.
func TestShingleBagIsBounded(t *testing.T) {
	// Each of these 8-byte shingles differs: exactly the bound, then one over.
	if b := newShingleBag(fuzzyLetters(maxBagShingles+7, 13)); b.sample != 0 || len(b.want) != maxBagShingles {
		t.Errorf("a body of exactly the bound was sampled (%b), holding %d", b.sample, len(b.want))
	}
	if b := newShingleBag(fuzzyLetters(maxBagShingles+8, 13)); b.sample != 1 {
		t.Errorf("a body one over the bound keeps a sample of %b, want every second shingle", b.sample)
	}
	// However large the request, the bag holds at most 65536 shingles.
	req := fuzzyLetters(2_000_000, 13)
	bag := newShingleBag(req)
	if len(bag.want) > 1<<16 || len(bag.slots) > 1<<16 {
		t.Fatalf("the bag holds %d shingles (%d slots), bound 65536", len(bag.want), len(bag.slots))
	}
	if len(bag.want) <= maxBagShingles/2 {
		t.Errorf("the bag keeps %d shingles of a bound of %d: a sample thinner than it need be", len(bag.want), maxBagShingles)
	}
	same, half, other := bag.similarity(req), bag.similarity(req[:1_000_000]), bag.similarity(fuzzyLetters(2_000_000, 17))
	if same != 1 || half < 0.45 || half > 0.55 || other > 0.01 {
		t.Errorf("similarity: same %v, its half %v, unrelated %v", same, half, other)
	}
}

// A ranking whose call ended stops where it is and picks nothing.
func TestRankingStopsWithItsContext(t *testing.T) {
	mocks := []*models.Mock{fuzzyMock("a", fuzzyLetters(8000, 3)), fuzzyMock("b", fuzzyLetters(8000, 5))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, req := range map[string]string{"by distance": fuzzyLetters(100, 7), "by shingles": fuzzyLetters(8000, 7)} {
		if ok, m, err := newHTTP().performFuzzyMatch(ctx, mocks, []byte(req)); ok || m != nil || !errors.Is(err, context.Canceled) {
			t.Errorf("%s: ok=%v mock=%v err=%v, want context.Canceled", name, ok, m, err)
		}
	}
	if _, _, err := findStringMatch(ctx, "x", mocks); !errors.Is(err, context.Canceled) {
		t.Errorf("findStringMatch: err=%v, want context.Canceled", err)
	}
}

// A heavy call waits for a turn and holds only its request meanwhile; a light
// call never does.
func TestHeavyCallsWaitForATurn(t *testing.T) {
	giveBack := takeAllTurns(t)
	db := heavyDB(t, 8, 200000)
	in := sent("/poll", jsonHeader, jsonQ(fuzzyLetters(200000, 3)))
	in.raw = in.body
	answered := make(chan error, 1)
	go func() {
		_, _, _, err := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, false, false)
		answered <- err
	}()
	select {
	case err := <-answered:
		t.Fatalf("a heavy call was matched with no turn free (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	light := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{recorded("a", "/poll", jsonHeader, jsonQ("aaaa")), recorded("b", "/poll", jsonHeader, jsonQ("bbbb"))}}
	lin := sent("/poll", jsonHeader, jsonQ("aaab"))
	lin.raw = lin.body
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, _ = newHTTP().match(context.Background(), lin, light, nil, nil, nil, false, false, false, false, false)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a light call waited for a heavy call's turn")
	}

	giveBack()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the heavy call: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the heavy call was not matched once a turn was free")
	}
}

func TestAdmit(t *testing.T) {
	if got, want := cap(heavyTurns()), runtime.GOMAXPROCS(0); got != want {
		t.Errorf("%d turns, want one for each of Go's %d CPUs", got, want)
	}
	takeAllTurns(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, end, err := admit(ctx, nil, heavyCallBytes); err != nil {
		t.Errorf("a call of exactly heavyCallBytes waited for a turn: %v", err)
	} else {
		end()
	}
	waited := make(chan error, 1)
	go func() {
		_, _, err := admit(ctx, nil, heavyCallBytes+1)
		waited <- err
	}()
	select {
	case err := <-waited:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a call one byte heavier: err=%v, want it to wait until its context ended", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call waiting for a turn did not end with its context")
	}
}

// A turn given back leaves no watch of its client running.
func TestATurnEndsItsWatch(t *testing.T) {
	_, proxy := tcpPair(t)
	base := runtime.NumGoroutine()
	_, end, err := admit(context.Background(), proxy, heavyCallBytes+1)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.NumGoroutine() <= base {
		t.Fatal("a heavy call with a client has no watch")
	}
	end()
	for start := time.Now(); runtime.NumGoroutine() > base; time.Sleep(time.Millisecond) {
		if time.Since(start) > 5*time.Second {
			t.Fatalf("%d goroutines after the turn ended, %d before it", runtime.NumGoroutine(), base)
		}
	}
}

// A call weighs its own body and the recordings of its method: a large
// request waits for a turn whatever it is compared with, and a POST takes
// none for large recordings of another method, which no pass reads.
func TestACallsWeight(t *testing.T) {
	giveBack := takeAllTurns(t)
	small := func() []*models.Mock {
		return []*models.Mock{recorded("a", "/poll", jsonHeader, jsonQ("aaaa")), recorded("b", "/poll", jsonHeader, jsonQ("bbbb"))}
	}
	call := func(body string, db *mockMemDb) <-chan error {
		in := sent("/poll", jsonHeader, body)
		in.raw = in.body
		done := make(chan error, 1)
		go func() {
			_, _, _, err := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, false, false)
			done <- err
		}()
		return done
	}

	gets := heavyDB(t, 8, 200000)
	for _, m := range gets.mocks {
		m.Spec.HTTPReq.Method = "GET"
	}
	light := &mockMemDb{updateUnFilteredReturn: true, mocks: append(small(), gets.mocks...)}
	select {
	case err := <-call(jsonQ("aaab"), light):
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a POST waited for a turn for recordings of another method")
	}

	heavy := call(jsonQ(fuzzyLetters(heavyCallBytes+1, 3)), &mockMemDb{updateUnFilteredReturn: true, mocks: small()})
	select {
	case err := <-heavy:
		t.Fatalf("a large request was matched with no turn free (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	giveBack()
	select {
	case err := <-heavy:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a large request was not matched once a turn was free")
	}
}

// A match its caller ended is that, not a client's end: only a watch on the
// client says the client left.
func TestAnEndedCallIsNoClientsEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := sent("/poll", jsonHeader, jsonQ("aaab"))
	db := heavyDB(t, 8, 200000)
	_, _, _, err := newHTTP().match(ctx, in, db, nil, nil, nil, false, false, false, false, false)
	if !errors.Is(err, context.Canceled) || errors.Is(err, errClientGone) {
		t.Errorf("err=%v, want context.Canceled and no client's end", err)
	}
}

func TestRecordedBytes(t *testing.T) {
	post, get := fuzzyMock("post", "12345"), fuzzyMock("get", "123")
	get.Spec.HTTPReq.Method = "GET"
	for _, tc := range []struct {
		method models.Method
		want   int64
	}{{"POST", 5}, {"GET", 3}, {"", 8}, {"PUT", 0}} {
		if got := recordedBytes([]*models.Mock{post, get}, tc.method); got != tc.want {
			t.Errorf("recordedBytes(%q) = %d, want %d", tc.method, got, tc.want)
		}
	}
}

// Calls whose clients gave up leave nothing behind: each of the concurrent
// identical heavy calls waiting for its turn ends with its client, and a call
// whose client leaves once its ranking has begun stops there, with nothing
// picked. No recording is spent on any of them, none is a miss, and the
// goroutines and heap of all of them go back to where they were. Where a
// socket's state cannot be read a heavy call does not end with its client, so
// there is nothing here to see.
func TestAbandonedCallsLeaveNothingBehind(t *testing.T) {
	_, probe := tcpPair(t)
	if _, known := pUtil.PeerGone(probe); !known {
		t.Skip("no socket's state can be read on this platform")
	}
	wire := fuzzyPost(jsonQ(fuzzyLetters(1<<20, 3)))
	t.Run("waiting for a turn", func(t *testing.T) {
		db := heavyDB(t, 8, 200000)
		giveBack := takeAllTurns(t)
		defer giveBack()
		heapBase, base := heapNow(), runtime.NumGoroutine()
		const calls = 32
		var hangUps []func()
		var ended []<-chan error
		for i := 0; i < calls; i++ {
			app, proxy := tcpPair(t)
			ended = append(ended, runDecode(t, proxy, func(ctx context.Context) error {
				return newHTTP().decodeHTTP(ctx, []byte(wire), proxy, nil, db, models.OutgoingOptions{})
			}))
			hangUps = append(hangUps, func() { _ = app.Close() })
		}
		// Long enough for every call to be waiting for its turn.
		time.Sleep(300 * time.Millisecond)
		for _, hangUp := range hangUps {
			hangUp()
		}
		deadline := time.After(5 * time.Second)
		for i, done := range ended {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("an abandoned call: %v", err)
				}
			case <-deadline:
				t.Fatalf("%d of %d abandoned calls still running 5 s after their clients left", calls-i, calls)
			}
		}
		settled(t, "abandoned calls", base, heapBase, 12<<20)
		if db.updateUnFilteredMockNew != nil {
			t.Errorf("a recording was spent on a call whose client left: %s", db.updateUnFilteredMockNew.Name)
		}
	})
	t.Run("in its match", func(t *testing.T) {
		db := heavyDB(t, 16, 1<<20)
		app, proxy := tcpPair(t)
		heapBase, base := heapNow(), runtime.NumGoroutine()
		began := false
		// The client leaves from inside the ranking, which then goes on as
		// a ranking would, for ten of the watch's polls.
		h, picks := onRanking(func() {
			began = true
			_ = app.Close()
			time.Sleep(time.Second)
		})
		done := runDecode(t, proxy, func(ctx context.Context) error {
			return h.decodeHTTP(ctx, []byte(wire), proxy, nil, db, models.OutgoingOptions{})
		})
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("a call whose client left in its match ended with %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("decodeHTTP still running 30 s after its client left")
		}
		if !began {
			t.Fatal("the call never reached its ranking")
		}
		if n := picks.Load(); n != 0 {
			t.Errorf("the ranking went on to pick a recording for a client that had left (%d picks)", n)
		}
		if db.updateUnFilteredMockNew != nil {
			t.Errorf("the match went on to spend %s on a client that had left", db.updateUnFilteredMockNew.Name)
		}
		settled(t, "a call cut in its ranking", base, heapBase, 12<<20)
	})
}

// Not knowing is not a client's end: where a client's socket cannot be read
// (net.Pipe here, and every socket on a platform with no such state) a heavy
// call that outlasts its watch's polls is ranked to its end and answered.
func TestAHeavyCallOutlastsAClientItCannotAsk(t *testing.T) {
	app, proxy := net.Pipe()
	defer func() { _ = app.Close(); _ = proxy.Close() }()
	if _, known := pUtil.PeerGone(proxy); known {
		t.Fatal("net.Pipe has no socket to ask")
	}
	// Three of its watch's polls.
	h, picks := onRanking(func() { time.Sleep(300 * time.Millisecond) })
	in := sent("/poll", jsonHeader, jsonQ(fuzzyLetters(200000, 3)))
	in.raw, in.client = in.body, proxy
	ok, m, _, err := h.match(context.Background(), in, heavyDB(t, 8, 200000), nil, nil, nil, false, false, false, false, false)
	if err != nil || !ok || m == nil {
		t.Fatalf("ok=%v mock=%v err=%v, want the call answered", ok, m, err)
	}
	if picks.Load() != 1 {
		t.Fatalf("%d rankings ran to a pick, want the one", picks.Load())
	}
}

// The passes before the ranking read the recordings' bodies too, so a heavy
// call they answer takes its turn as well. A turn taken only at the ranking
// leaves every such pass unbounded: 256 clients then held the heap at over
// 1.3 GB, with no call answered.
func TestAHeavyCallAnsweredBeforeItsRankingTakesATurn(t *testing.T) {
	db := heavyDB(t, 8, 200000)
	db.mocks = append(db.mocks, recorded("only", "/poll", jsonHeader, `{"r":"x"}`))
	for _, tc := range []struct{ name, body, want string }{
		{"by its exact body", db.mocks[3].Spec.HTTPReq.Body, db.mocks[3].Name},
		{"by its only body match", `{"r":"y"}`, "only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			giveBack := takeAllTurns(t)
			in := sent("/poll", jsonHeader, tc.body)
			in.raw = in.body
			picked := make(chan *models.Mock, 1)
			go func() {
				_, m, _, _ := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, false, false)
				picked <- m
			}()
			select {
			case m := <-picked:
				t.Fatalf("answered with %v with no turn free", m)
			case <-time.After(300 * time.Millisecond):
			}
			giveBack()
			select {
			case m := <-picked:
				if m == nil || m.Name != tc.want {
					t.Fatalf("answered with %v, want %s", m, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("not answered once a turn was free")
			}
		})
	}
}

// What a pair shares at its ends costs a ranking nothing: two 200 KB bodies
// four bytes apart in the middle, a poller's drifting timestamp, fit the
// bound and are ranked on those bytes. Ranked whole, they take over a minute.
func TestAPairSharingItsEndsIsRankedOnWhatDiffers(t *testing.T) {
	ends := fuzzyLetters(100000, 3)
	body := func(drift string) string { return ends + drift + ends }
	mocks := []*models.Mock{fuzzyMock("far", body("2222")), fuzzyMock("near", body("1112"))}
	if !levenshteinFits(context.Background(), body("1111"), mocks) {
		t.Fatal("the pair does not fit the bound: this row ranks nothing by distance")
	}
	picked := make(chan *models.Mock, 1)
	go func() {
		_, m := newHTTP().PerformFuzzyMatch(mocks, []byte(body("1111")))
		picked <- m
	}()
	select {
	case m := <-picked:
		if m == nil || m.Name != "near" {
			t.Fatalf("picked %v, want near", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("two 200 KB bodies four bytes apart took over 5 s to rank")
	}
}

// heapAtEachRead is a context whose Err, which a ranking asks before it reads
// each recording, notes the heap left live there, where a ranking that holds
// one recording at a time holds none.
type heapAtEachRead struct {
	context.Context
	live []uint64
}

func (c *heapAtEachRead) Err() error {
	c.live = append(c.live, heapLeft())
	return nil
}

// heapLeft is the heap live after two collections: the first leaves what a
// sync.Pool keeps (json's encoder keeps its last buffer, a body's size), the
// second frees it.
func heapLeft() uint64 {
	runtime.GC()
	return liveHeap()
}

// A ranking holds one recording at a time, by its shingles and by Levenshtein
// alike. Its noisy recordings are each read as a 4 MB copy of the body, and
// between two reads it holds only its own overhead: its request's copy, and
// in the shingle pass the request's bag of shingles. The bound is the heap
// before the ranking, plus that overhead and half a copy, each measured here.
// A ranking that kept the copy it read while it read the next, two at once,
// is half a copy over it; one that read them all first, 7.5 copies.
func TestARankingHoldsOneRecordingAtATime(t *testing.T) {
	const recordings, size = 8, 4 << 20
	letters := fuzzyLetters(size, 5)
	body := `{"a":"` + letters + `","token":"RRRR"}`
	mocks := make([]*models.Mock, recordings)
	for i := range mocks {
		mocks[i] = fuzzyMock(strconv.Itoa(i), body)
		mocks[i].Noise = []string{"^R+$"}
	}
	// Past 65535 characters a pair is ranked by its shingles, whatever the
	// request's size: a small request keeps the overhead apart from a copy.
	byShingles := []byte(`{"a":"` + fuzzyLetters(1<<16, 7) + `"}`)
	// One letter away from each recording once its noise is stripped: each
	// pair trims to that letter (trimCommonEnds), so it fits and is ranked by
	// Levenshtein. Its overhead, the request's copy, is a copy's size.
	byLevenshtein := []byte(`{"a":"` + letters + `"}`)
	c := &byLevenshtein[len(byLevenshtein)/2]
	*c = 'a' + (*c-'a'+1)%26

	t.Run("by its shingles", func(t *testing.T) {
		holdsOneAtATime(t, mocks, byShingles, size, false)
	})
	t.Run("by Levenshtein", func(t *testing.T) {
		holdsOneAtATime(t, mocks, byLevenshtein, size, true)
	})
}

// holdsOneAtATime ranks req against mocks, noisy recordings of size bytes, by
// the pass byLevenshtein names, and fails if the heap at one of its reads is
// over the bound TestARankingHoldsOneRecordingAtATime states.
func holdsOneAtATime(t *testing.T, mocks []*models.Mock, req []byte, size uint64, byLevenshtein bool) {
	if fits := levenshteinFits(context.Background(), string(req), mocks); fits != byLevenshtein {
		t.Fatalf("levenshteinFits says %v: the row would measure the other pass", fits)
	}
	// The shingle pass reads each recording once, after levenshteinFits read
	// the first; the Levenshtein pass reads each twice, in levenshteinFits and
	// then in findStringMatch. The overhead is under a copy in the shingle
	// pass, and in the Levenshtein pass the request's copy, within the eighth
	// of a copy that a copy's own measure is allowed.
	reads, maxOverhead := len(mocks), size
	if byLevenshtein {
		reads, maxOverhead = 2*len(mocks), uint64(len(req))+size/8
	}

	base := heapLeft()
	reqCopy := string(req)
	var bag *shingleBag
	if !byLevenshtein {
		bag = newShingleBag(reqCopy)
	}
	overhead := heapLeft() - base
	held := fuzzyBody(mocks[0])
	one := heapLeft() - base - overhead
	runtime.KeepAlive(reqCopy)
	runtime.KeepAlive(bag)
	runtime.KeepAlive(held)
	if one < size || one > size+size/8 || overhead > maxOverhead {
		t.Fatalf("a copy measured %.1f MB, want its body's %.1f, and the ranking's overhead %.1f MB, want at most %.1f: the row cannot tell one held from none",
			mb(one), mb(uint64(len(held))), mb(overhead), mb(maxOverhead))
	}
	bound := base + overhead + one/2
	t.Logf("before the ranking %.1f MB; its overhead %.1f MB; a copy %.1f MB; bound %.1f MB", mb(base), mb(overhead), mb(one), mb(bound))

	ctx := &heapAtEachRead{Context: context.Background(), live: make([]uint64, 0, 4*len(mocks))}
	done := make(chan error, 1)
	go func() {
		ok, _, err := newHTTP().performFuzzyMatch(ctx, mocks, req)
		if err == nil && !ok {
			err = errors.New("no recording picked")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the ranking of %d recordings of 4 MB took over 60 s", len(mocks))
	}
	// The request stays live through the ranking, as a client's does, so
	// that its copy is the overhead measured above.
	runtime.KeepAlive(req)
	if len(ctx.live) < reads {
		t.Fatalf("the ranking asked its context %d times for %d reads: the row measured nothing", len(ctx.live), reads)
	}
	for i, live := range ctx.live {
		if live > bound {
			t.Fatalf("at its read %d the heap was %.1f MB, %.1f copies over its overhead, the bound %.1f: it held the recordings it read",
				i, mb(live), float64(live-base-overhead)/float64(one), mb(bound))
		}
	}
	t.Logf("at its %d reads the heap was at most %.1f MB", len(ctx.live), mb(slices.Max(ctx.live)))
}

func mb(n uint64) float64 { return float64(n) / (1 << 20) }

// failsFirstClaim refuses its first claim, as a store does when another call
// took the recording first: the match reads the store and picks again.
type failsFirstClaim struct {
	*mockMemDb
	claims int
}

func (s *failsFirstClaim) UpdateUnFilteredMock(old, updated *models.Mock) bool {
	s.claims++
	return s.claims > 1 && s.mockMemDb.UpdateUnFilteredMock(old, updated)
}

// A heavy call that picks again keeps the turn it has: asking for another
// beside it, with none free, it would wait on itself.
func TestAHeavyCallTakesOneTurn(t *testing.T) {
	turns := heavyTurns()
	takeTurns(t, cap(turns)-1)
	db := &failsFirstClaim{mockMemDb: heavyDB(t, 8, 200000)}
	in := sent("/poll", jsonHeader, jsonQ(fuzzyLetters(200000, 3)))
	in.raw = in.body
	done := make(chan error, 1)
	go func() {
		ok, _, _, err := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, false, false)
		if err == nil && !ok {
			err = errors.New("no recording")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if db.claims != 2 {
			t.Fatalf("%d claims, want the refused one and the one after", db.claims)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a heavy call that picked again never ended")
	}
	if len(turns) != cap(turns)-1 {
		t.Fatalf("%d turns held after the call, want the row's %d: the call kept one", len(turns), cap(turns)-1)
	}
}

// A caller of PerformFuzzyMatch, with no client to watch, takes its turn too.
func TestPerformFuzzyMatchTakesATurn(t *testing.T) {
	giveBack := takeAllTurns(t)
	db := heavyDB(t, 8, 200000)
	done := make(chan bool, 1)
	go func() {
		ok, _ := newHTTP().PerformFuzzyMatch(db.mocks, []byte(jsonQ(fuzzyLetters(200000, 3))))
		done <- ok
	}()
	select {
	case <-done:
		t.Fatal("a heavy ranking ran with no turn free")
	case <-time.After(300 * time.Millisecond):
	}
	giveBack()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("no recording picked")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ranking did not run once a turn was free")
	}
}

// A client that read its answer leaves no body held by the call, on a match
// and on a miss: over the calls below the heap holds no more than a call or
// two's worth of the bodies that went through it.
func TestAnsweredCallsHoldNoBody(t *testing.T) {
	const calls = 32
	body := func(i int, key string) string { return `{"` + key + `":"` + fuzzyLetters(1<<20, uint32(i+1)) + `"}` }
	for _, tc := range []struct {
		name string
		key  string
		want string
	}{
		{"a match", "q", "200"},
		{"a miss", "other", "502"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
				recorded("a", "/poll", jsonHeader, jsonQ(fuzzyLetters(300, 5))), recorded("b", "/poll", jsonHeader, jsonQ(fuzzyLetters(300, 7)))}}
			heapBase, base := heapNow(), runtime.NumGoroutine()
			for i := 0; i < calls; i++ {
				wire := fuzzyPost(body(i, tc.key))
				app, proxy := tcpPair(t)
				done := make(chan error, 1)
				ctx, cancel := context.WithCancel(context.Background())
				go func() { done <- newHTTP().decodeHTTP(ctx, []byte(wire), proxy, nil, db, models.OutgoingOptions{}) }()
				line, err := bufio.NewReader(app).ReadString('\n')
				if err != nil || !strings.Contains(line, tc.want) {
					t.Fatalf("call %d: read %q, %v, want %s", i, line, err, tc.want)
				}
				_ = app.Close()
				<-done
				cancel()
				_ = proxy.Close()
			}
			settled(t, tc.name, base, heapBase, 12<<20)
		})
	}
}

func TestLevenshteinFits(t *testing.T) {
	a := func(n int) string { return strings.Repeat("a", n) }
	b := func(n int) *models.Mock { return fuzzyMock("b", strings.Repeat("b", n)) }
	for _, tc := range []struct {
		name  string
		req   string
		mocks []*models.Mock
		want  bool
	}{
		{"no candidates", a(100), nil, true},
		{"exactly the bound", a(4096), []*models.Mock{b(4096)}, true},
		{"one step over the bound", a(4096), []*models.Mock{b(4096), b(1)}, false},
		{"the bound is summed over candidates", a(4096), []*models.Mock{b(4096), b(4096)}, false},
		{"one large candidate", a(200000), []*models.Mock{b(200000)}, false},
		{"a request of 65535 characters", a(65535), []*models.Mock{b(0)}, true},
		{"a request of 65536 characters", a(65536), []*models.Mock{b(0)}, false},
		{"a candidate of 65535 characters", "", []*models.Mock{b(65535)}, true},
		{"a candidate of 65536 characters", "", []*models.Mock{b(65536)}, false},
		{"65535 by 65535 characters", a(65535), []*models.Mock{b(65535)}, false},
		{"only what differs counts", a(200000) + "x" + a(200000), []*models.Mock{fuzzyMock("y", a(200000)+"y"+a(200000))}, true},
		{"a non-ASCII recording is not ranked by it", a(4096), []*models.Mock{b(4096), fuzzyMock("u", strings.Repeat("\u00e9", 4096))}, true},
	} {
		if got := levenshteinFits(context.Background(), tc.req, tc.mocks); got != tc.want {
			t.Errorf("%s: levenshteinFits = %v, want %v", tc.name, got, tc.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if levenshteinFits(ctx, a(10), []*models.Mock{b(10)}) {
		t.Error("levenshteinFits went on past its context's end")
	}
}

func TestTrimCommonEnds(t *testing.T) {
	for _, tc := range []struct{ a, b, wantA, wantB string }{
		{"", "", "", ""},
		{"abc", "abc", "", ""},
		{"abcXdef", "abcYYdef", "X", "YY"},
		{"aaaa", "aa", "aa", ""},
		{"ab", "abab", "", "ab"},
		{"xabc", "yabc", "x", "y"},
		{"abcx", "abcy", "x", "y"},
	} {
		if a, b := trimCommonEnds(tc.a, tc.b); a != tc.wantA || b != tc.wantB {
			t.Errorf("trimCommonEnds(%q, %q) = %q, %q, want %q, %q", tc.a, tc.b, a, b, tc.wantA, tc.wantB)
		}
	}
}
