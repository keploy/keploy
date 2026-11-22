package learn

import (
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// viewedMock is jobsMock named name, d after it: another recording of the
// set where d is not 0.
func viewedMock(name, id, n string, d time.Duration) *models.Mock {
	m := jobsMock(id, n)
	m.Name = name
	m.Spec.ReqTimestampMock = m.Spec.ReqTimestampMock.Add(d)
	m.Spec.ResTimestampMock = m.Spec.ResTimestampMock.Add(d)
	return m
}

// countedView is a view's loader that counts its loads in n.
func countedView(n *atomic.Int64, view *models.Mock) func() (*models.Mock, error) {
	return func() (*models.Mock, error) {
		n.Add(1)
		return view, nil
	}
}

// servedViewVerdict is what a call learns of a value whose new word may
// stand in r (ClassifyGiven), with the text of each view r was read with.
func servedViewVerdict(dc *DriftCache, r Raw) string {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	got := ClassifyGiven(dc, []string{path}, live, rec, eRecAt, nil, append(eStartup(), r), nil, nil, nil)
	var b strings.Builder
	b.WriteString(string(got[0].Kind) + ": " + got[0].Why)
	if r.live != nil {
		b.WriteString(" | view " + r.live.rText)
	}
	for _, v := range r.views {
		b.WriteString(" | views " + v.rText)
	}
	return b.String()
}

// The views F's serves gave of a recording (Tracker.ServedMockLive) are
// read once for the set (Order.ServedView, setDerived.views), where every
// call that reads the recording read each of them again: the work is the
// views the set's calls ask for, not those views times the calls. Past the
// set's budget for them (here none) each call reads them again, as each did
// before, and learns the same: what every call learns, and the views it
// reads, are the same either way, those of two recordings and of one
// recording's two views each its own (a view keyed by its recording alone,
// or by its place alone, would read another's). Handlers reading one
// recording at once load each view once. The views kept are counted on the
// set's budget, and let go with the set: a handler still reading the set
// that is over reads each view again at each call, and the set keeps none.
func TestOrderServedViewReadsEachViewOnceForTheSet(t *testing.T) {
	defer func(b int64) { viewBudget = b }(viewBudget)
	m1 := jobsMock(chanceSrcRec, "5")
	m2 := viewedMock("mock-7", chanceSrcRec, "5", time.Millisecond)
	const tests, callsPerTest, handlers = 3, 2, 8
	type run struct {
		verdicts []string
		// loads are the views' loads before the set was over, and first
		// those of its first call (its handlers at once).
		loads, first int64
	}
	read := func(budget int64) run {
		viewBudget = budget
		var loads atomic.Int64
		tr := NewTracker()
		tr.Begin()
		tr.ServedMock(m1)
		tr.ServedMockLive(m1, "a", countedView(&loads, viewedMock("mock-6", "6c7d", "9", 0)))
		tr.ServedMock(m2)
		tr.ServedMockLive(m2, "a", countedView(&loads, viewedMock("mock-7", "4e1f", "9", time.Millisecond)))
		tr.ServedMock(m2)
		tr.ServedMockLive(m2, "b", countedView(&loads, viewedMock("mock-7", "6c7d", "9", time.Millisecond)))
		dc := noSet(clockedDrifts())
		served := func(o Order, m *models.Mock) string {
			first, last := o.Of(m)
			return servedViewVerdict(dc, o.ServedView(m, dc.LazyRawOf(m, true, nil).ServedIn(first, last)))
		}
		var out run
		for i := 0; i < tests; i++ {
			if i > 0 {
				tr.End()
				tr.Begin()
			}
			for j := 0; j < callsPerTest; j++ {
				o := tr.Placed().Order
				if i == 0 && j == 0 {
					got := make([][2]string, handlers)
					var wg sync.WaitGroup
					for h := range got {
						wg.Add(1)
						go func() {
							defer wg.Done()
							got[h] = [2]string{served(o, m1), served(o, m2)}
						}()
					}
					wg.Wait()
					for h := 1; h < handlers; h++ {
						if got[h] != got[0] {
							t.Errorf("handlers reading the same views at once learned %q and %q", got[0], got[h])
						}
					}
					out.verdicts = append(out.verdicts, got[0][0], got[0][1])
					out.first = loads.Load()
					continue
				}
				out.verdicts = append(out.verdicts, served(o, m1), served(o, m2))
			}
		}
		s := tr.derived
		s.mu.Lock()
		used, kept, run := s.budget.used, s.views.kept.used, s.run
		s.mu.Unlock()
		if tiers := s.usedBytes(); used != tiers+run || budget != 0 && kept == 0 {
			t.Errorf("the set's budget counts %d bytes, its tiers %d (%d of views) and its run %d: want the views kept on the set's budget", used, tiers, kept, run)
		}
		// A handler still reading the set that is over.
		o := tr.Placed().Order
		before := loads.Load()
		tr.NewSet()
		for j := 0; j < 2; j++ {
			out.verdicts = append(out.verdicts, served(o, m1))
		}
		if n := loads.Load() - before; n != 2 {
			t.Errorf("a handler reading a set that is over read its recording's view twice and loaded it %d times: want twice, its set keeping none", n)
		}
		if u := s.usedBytes(); u != 0 {
			t.Errorf("a set that is over keeps %d bytes: want none", u)
		}
		out.loads = before
		return out
	}
	kept, again := read(noCap), read(0)
	const views = 3
	if kept.loads != views {
		t.Errorf("%d calls of the set (%d handlers at once at its first) read %d views of 2 recordings and loaded them %d times: want each once for the set", tests*callsPerTest, handlers, views, kept.loads)
	}
	if n := again.loads - again.first; again.first < views || n != (tests*callsPerTest-1)*views {
		t.Errorf("past the set's budget for its views, %d calls after the first read %d views and loaded them %d times (the first's handlers %d): want each at each call (%d)", tests*callsPerTest-1, views, n, again.first, (tests*callsPerTest-1)*views)
	}
	if len(kept.verdicts) != len(again.verdicts) {
		t.Fatalf("%d verdicts with the views kept, %d with none", len(kept.verdicts), len(again.verdicts))
	}
	for i := range kept.verdicts {
		if kept.verdicts[i] != again.verdicts[i] {
			t.Errorf("call %d learned %q with the set's views kept, %q with each read again", i, kept.verdicts[i], again.verdicts[i])
		}
	}
	for i, want := range []string{"as record's full round gave it", "serves that gave the application different views of it"} {
		if v := kept.verdicts[i]; !strings.HasPrefix(v, string(KindUnaccounted)) || !strings.Contains(v, want) || !strings.Contains(v, "| view") {
			t.Errorf("recording %d: learned %q; want refused (%q), read with its views", i+1, v, want)
		}
	}
}

// A view whose loader failed (an answer parked on disk not read back) is
// kept by none: the next call loads it again, as each call did, and reads
// it once it loads; a refusal at one call is not kept for the set's. A
// recording's payload that could not be loaded is kept (bodyStore): the
// read bit a rule asks for stays, and it is loaded once.
func TestOrderServedViewLoadsAgainAViewThatCouldNotBeLoaded(t *testing.T) {
	m := jobsMock(chanceSrcRec, "5")
	view := viewedMock("mock-6", "6c7d", "9", 0)
	var loads atomic.Int64
	tr := NewTracker()
	tr.Begin()
	tr.ServedMock(m)
	tr.ServedMockLive(m, "a", func() (*models.Mock, error) {
		if loads.Add(1) == 1 {
			return nil, errors.New("the parked answer is gone")
		}
		return view, nil
	})
	dc := noSet(clockedDrifts())
	read := func() string {
		o := tr.Placed().Order
		first, last := o.Of(m)
		return servedViewVerdict(dc, o.ServedView(m, dc.LazyRawOf(m, true, nil).ServedIn(first, last)))
	}
	if v := read(); !strings.Contains(v, "where keploy could not read what it gave the application") {
		t.Errorf("a view that could not be loaded: learned %q; want refused as one keploy could not read", v)
	}
	for i := 0; i < 2; i++ {
		if v := read(); !strings.Contains(v, "as record's full round gave it") || !strings.Contains(v, "| view") {
			t.Errorf("call %d after the view's load failed: learned %q; want it read with the view", i+2, v)
		}
	}
	if n := loads.Load(); n != 2 {
		t.Errorf("a view that failed to load once was loaded %d times by three calls: want twice, kept once read", n)
	}
	gone := func() (*models.Mock, error) { return nil, errors.New("the parked answer is gone") }
	bodies := newBodyStore(1 << 20)
	for i := 0; i < 2; i++ {
		if b := bodies.get("mock-6", "mock-6", gone); b.read {
			t.Fatalf("a payload that could not be loaded reads as read")
		}
	}
	if bodies.loads != 1 {
		t.Errorf("a recording's payload that could not be loaded was loaded %d times by two reads: want once, its read bit kept", bodies.loads)
	}
}

// The set's budget lets go of the words first, then of the payloads, and
// of the views last (setDerived.views): only what the words and the
// payloads could not free.
func TestTheSetLetsGoOfItsViewsLast(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 1 << 30
	const size = 1 << 10
	const each = size + slotBytes + 1 // a part and its slot (tier.keep)
	for _, c := range []struct {
		name                string
		need                int64
		words, bodies, view bool // still kept
	}{
		{"the words free enough", each, false, true, true},
		{"the words and payloads free enough", each + 1, false, false, true},
		{"they do not", 2*each + 1, false, false, false},
	} {
		s := newSetDerived()
		s.mu.Lock()
		s.words.keep("w", partTokens, size, func(*wordParts) {})
		s.bodies.kept.keep("b", partBody, size, func(*rawBody) {})
		s.views.kept.keep("v", partBody, size, func(*rawBody) {})
		s.budget.letGo(c.need)
		words, bodies, view := s.words.find("w", partTokens) != nil, s.bodies.kept.find("b", partBody) != nil, s.views.kept.find("v", partBody) != nil
		s.mu.Unlock()
		if words != c.words || bodies != c.bodies || view != c.view {
			t.Errorf("%s: kept words %v, a payload %v, a view %v; want %v, %v, %v", c.name, words, bodies, view, c.words, c.bodies, c.view)
		}
	}
}

// The view Order.ServedView reads through the set's store of views
// (Order.viewRead), at the read that loads it and at a later one that finds
// it kept, is the view each call read before the set kept views (viewRaw,
// read by Raw.viewOf): its payload, text, fields, stamps (a Date header)
// and clock reads (a time of its body), which the learn step blanks or
// reads (DriftCache.searched), and the rest of it as a rule reads it. The
// view of a connection's recorded start-up (a Postgres session, isStartUp)
// reads as no start-up, as before. Two recordings of one name, served at
// two moments, each read their own view: the store keys a view by its
// recording's identity (MockID), not by its name. A read decodes nothing
// but the view the store loads, once for the set: never the recording,
// which no read decoded before either.
func TestOrderServedViewReadsTheViewEachCallReadBefore(t *testing.T) {
	asked := time.Date(2026, 10, 22, 11, 14, 13, 0, time.UTC)
	mk := func(at, in time.Duration, n int) *models.Mock {
		from := asked.Add(at)
		return &models.Mock{Name: "mock-3", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata: map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimeSession},
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "/now"},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Date": from.Add(2 * time.Second).Format(http.TimeFormat)},
				Body: `{"at":"` + from.Add(in).Format(time.RFC3339Nano) + `","n":` + strconv.Itoa(n) + `}`},
			ReqTimestampMock: from, ResTimestampMock: from.Add(3 * time.Second),
		}}
	}
	// session is a connection's recorded start-up with backend pid pid: F
	// gave the application another connection's (another pid and key).
	session := func(pid int32) *models.Mock {
		from := asked.Add(2 * time.Minute)
		return &models.Mock{Name: "mock-4", Kind: models.PostgresV3, Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"},
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeSession, Session: &models.PostgresV3SessionSpec{
				ServerVersion: "15.2", BackendProcessID: pid, BackendSecretKey: 7000 + pid,
			}},
			ReqTimestampMock: from, ResTimestampMock: from.Add(time.Millisecond),
		}}
	}
	recs := []*models.Mock{mk(0, 1500*time.Millisecond, 1), mk(time.Minute, 1500*time.Millisecond, 1), session(81)}
	views := []*models.Mock{mk(0, 2250*time.Millisecond, 2), mk(time.Minute, 2750*time.Millisecond, 3), session(82)}
	loader := func(i int) func() (*models.Mock, error) {
		return func() (*models.Mock, error) { return views[i], nil }
	}
	tr := NewTracker()
	tr.Begin()
	for i, m := range recs {
		tr.ServedMock(m)
		tr.ServedMockLive(m, "a", loader(i))
	}
	o := tr.Placed().Order
	searched := func(x *Raw) [2]string { return noSet(clockedDrifts()).searched(*x, false) }
	// decoded runs read and says how many payloads it decoded
	// (payloadDecodes) and how many views the store loaded meanwhile.
	decoded := func(read func()) (n int64, loads int) {
		d, l := payloadDecodes.Load(), o.views.loads
		read()
		return payloadDecodes.Load() - d, o.views.loads - l
	}
	if !isStartUp(recs[2]) {
		t.Fatal("the row's third recording is no connection start-up: want one")
	}
	for i, m := range recs {
		r := RawOf(m, true)
		before, ok := r.viewOf(viewRaw(m, loader(i)))
		if !ok {
			t.Fatalf("recording %d: its view was not read as before", i+1)
		}
		want := before.payload()
		if len(want.fields) == 0 {
			t.Fatalf("recording %d: its view has no fields: want some", i+1)
		}
		if isStartUp(m) {
			if before.startUp {
				t.Fatalf("recording %d, a connection's start-up: the old path read its view as a start-up; want it read as none", i+1)
			}
		} else if len(want.stamps) == 0 || len(want.answerClock) == 0 {
			t.Fatalf("recording %d: its view has %d stamps and %d clock reads: want some of each", i+1, len(want.stamps), len(want.answerClock))
		}
		for _, read := range []string{"loads it", "finds it kept"} {
			var v Raw
			if n, loads := decoded(func() { v = o.viewRead(m, MockID(m), 0, loader(i)) }); n != int64(loads) {
				t.Errorf("recording %d, the read that %s: decoded %d payloads and loaded %d views: want only the views it loads decoded, never the recording", i+1, read, n, loads)
			}
			got, ok := r.viewOf(v)
			if !ok {
				t.Errorf("recording %d, the read that %s: the view was not read; before it was", i+1, read)
				continue
			}
			if !reflect.DeepEqual(got.payload(), want) {
				t.Errorf("recording %d, the read that %s: read the view %+v; before %+v", i+1, read, got.payload(), want)
				continue
			}
			if !reflect.DeepEqual(*got, *before) {
				t.Errorf("recording %d, the read that %s: read the view as %+v; before %+v", i+1, read, *got, *before)
			}
			if g, w := searched(got), searched(before); g != w {
				t.Errorf("recording %d, the read that %s: searched %q; before %q", i+1, read, g, w)
			}
		}
		first, last := o.Of(m)
		in := r.ServedIn(first, last)
		var got Raw
		if n, loads := decoded(func() { got = o.ServedView(m, in) }); n != 0 || loads != 0 {
			t.Errorf("recording %d: ServedView of a view the set keeps decoded %d payloads and loaded %d views: want none", i+1, n, loads)
		}
		if got.live == nil || !reflect.DeepEqual(*got.live, *before) {
			t.Errorf("recording %d: ServedView read another view than before", i+1)
		}
	}
	if n := o.views.loads; n != len(recs) {
		t.Errorf("%d recordings' views read 3 times each were loaded %d times: want each once", len(recs), n)
	}
}
