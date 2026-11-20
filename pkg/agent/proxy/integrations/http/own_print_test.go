package http

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
)

// names is the names of ms, in their order.
func names(ms []*models.Mock) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// counts is ownPrints, livePrints and sameCallChecks as they stand.
func counts() [3]int64 {
	return [3]int64{ownPrints.Load(), livePrints.Load(), sameCallChecks.Load()}
}

// since is what each of counts grew by since before.
func since(before [3]int64) [3]int64 {
	now := counts()
	return [3]int64{now[0] - before[0], now[1] - before[1], now[2] - before[2]}
}

// Of the recordings a call is compared with, only one whose print is the
// call's, or says nothing (a value the obfuscator redacted), is compared
// with it: one that differs at a value, a type, an array's order or length,
// or a field only one side holds, is another call's by its print alone.
// Each recording is printed once, and the call once per call; the call's
// own recordings are found as the comparison finds them.
func TestARecordingWhosePrintIsNotTheCallsIsNotComparedWithIt(t *testing.T) {
	setProven(t, false)
	h := newHTTP()
	db := searchRecordings(t, h, nil,
		`{"q":"bob","ts":1,"page":{"n":1,"tags":["a","b"]}}`,
		`{"q":"ada","ts":1,"page":{"n":"1","tags":["a","b"]}}`,
		`{"q":"ada","ts":1,"page":{"n":1,"tags":["b","a"]}}`,
		`{"q":"ada","ts":1,"page":{"n":1,"tags":["a"]}}`,
		`{"q":"ada","ts":1,"page":{"n":1,"tags":["a","b"]},"admin":true}`,
		`{"q":"ada","ts":1,"page":{"n":1}}`,
		`{"q":"ada","ts":1,"page":{"n":1,"tags":["a","b"]}}`,
		`{"q":"REDACTED-1","ts":1,"page":{"n":1,"tags":["a","b"]}}`,
	)
	db.mocks[7].Noise = []string{"^REDACTED-[0-9]+$"}
	ts := map[string][]string{"ts": {}}
	call := []byte(`{"page":{"tags":["a","b"],"n":1},"ts":9,"q":"ada"}`)

	before := counts()
	own := h.sameCallBodies(db.mocks, mocknoise.NewJSONBody(call), ts)
	if want := []string{"mock-search-7", "mock-search-8"}; !slices.Equal(names(own), want) {
		t.Fatalf("the call's own recordings are %q, want %q", names(own), want)
	}
	if got, want := since(before), [3]int64{8, 1, 2}; got != want {
		t.Fatalf("the first call printed %d recordings and %d calls, and compared %d recordings; want %v", got[0], got[1], got[2], want)
	}
	before = counts()
	own = h.sameCallBodies(db.mocks, mocknoise.NewJSONBody(call), ts)
	if got, want := since(before), [3]int64{0, 1, 2}; got != want || len(own) != 2 {
		t.Fatalf("the second call printed %d recordings and %d calls, and compared %d recordings, with %d own; want %v and 2", got[0], got[1], got[2], len(own), want)
	}
}

// A call served from a pool of recordings of its method and path, none of
// which is its own, is compared with none of them: each is another call's
// by its print, and the call is served the closest and reported so.
func TestAPoolOfOtherCallsRecordingsIsComparedByPrintsAlone(t *testing.T) {
	setProven(t, false)
	h := newHTTP()
	var bodies []string
	for i := range 50 {
		bodies = append(bodies, `{"q":"user-`+strconv.Itoa(i)+`","first":`+strconv.Itoa(i%9)+`}`)
	}
	db := searchRecordings(t, h, nil, bodies...)
	before := counts()
	got := serveReportedWith(t, h, db, models.OutgoingOptions{}, searchCall(`{"q":"user-1000","first":1}`))
	if a := got.answers(); got.status != 200 || len(a) != 1 || a[0].Answer != models.AnswerClosestBody {
		t.Fatalf("the call got %d %q with answer reports %+v, want one %q", got.status, got.body, a, models.AnswerClosestBody)
	}
	if c := since(before); c[2] != 0 || c[0] != 50 {
		t.Fatalf("the call printed %d recordings and compared %d, want 50 and none", c[0], c[2])
	}
}

// A recording's print is taken again where what it was printed under
// changed: keploy.yml's request-body noise, the noise the record learned on
// it, its redacted values, its body. So each change makes the recording
// the call's own as the comparison reads it.
func TestARecordingIsPrintedAgainUnderOtherNoise(t *testing.T) {
	setProven(t, false)
	call := []byte(`{"q":"ada","ts":3,"pw":"hunter2"}`)
	for _, tc := range []struct {
		name string
		// first sets the first recording up before the first call, user
		// is keploy.yml's noise, and change changes one of them.
		first  func(m *models.Mock)
		user   map[string][]string
		change func(m *models.Mock, user *map[string][]string)
	}{
		{name: "keploy.yml's request-body noise",
			change: func(_ *models.Mock, user *map[string][]string) { *user = map[string][]string{"ts": {}} }},
		{name: "the noise the record learned",
			change: func(m *models.Mock, _ *map[string][]string) {
				m.Spec.ReqBodyNoise = map[string][]string{"body.ts": {}}
			}},
		{name: "another learned field", first: func(m *models.Mock) { m.Spec.ReqBodyNoise = map[string][]string{"body.q": {}} },
			change: func(m *models.Mock, _ *map[string][]string) {
				m.Spec.ReqBodyNoise = map[string][]string{"body.ts": {}}
			}},
		{name: "its redacted values", user: map[string][]string{"ts": {}}, first: func(m *models.Mock) { m.Spec.HTTPReq.Body = `{"q":"ada","ts":1,"pw":"REDACTED-1"}` },
			change: func(m *models.Mock, _ *map[string][]string) { m.Noise = []string{"^REDACTED-[0-9]+$"} }},
		{name: "its body", user: map[string][]string{"ts": {}}, first: func(m *models.Mock) { m.Spec.HTTPReq.Body = `{"q":"bob","ts":1,"pw":"hunter2"}` },
			change: func(m *models.Mock, _ *map[string][]string) {
				m.Spec.HTTPReq.Body = `{"q":"ada","ts":1,"pw":"hunter2"}`
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHTTP()
			db := searchRecordings(t, h, nil, `{"q":"ada","ts":1,"pw":"hunter2"}`, `{"q":"alan","ts":2,"pw":"x"}`)
			m := db.mocks[0]
			if tc.first != nil {
				tc.first(m)
			}
			user := tc.user
			if own := h.sameCallBodies(db.mocks, mocknoise.NewJSONBody(call), user); len(own) != 0 {
				t.Fatalf("before the change the call's own recordings are %q, want none", names(own))
			}
			tc.change(m, &user)
			if own := h.sameCallBodies(db.mocks, mocknoise.NewJSONBody(call), user); !slices.Equal(names(own), []string{"mock-search-1"}) {
				t.Fatalf("after the change the call's own recordings are %q, want mock-search-1", names(own))
			}
		})
	}
}

// A call is printed once for each noise the recordings it is compared with
// learned: recordings that learned the same fields share its print.
func TestACallIsPrintedOnceForEachLearnedNoise(t *testing.T) {
	setProven(t, false)
	h := newHTTP()
	db := searchRecordings(t, h, nil, `{"q":"a","ts":1,"n":1}`, `{"q":"b","ts":1,"n":1}`, `{"q":"c","ts":1,"n":1}`, `{"q":"d","ts":1,"n":1}`, `{"q":"e","ts":1,"n":1}`)
	db.mocks[0].Spec.ReqBodyNoise = map[string][]string{"body.ts": {}}
	db.mocks[1].Spec.ReqBodyNoise = map[string][]string{"body.ts": {}}
	db.mocks[3].Spec.ReqBodyNoise = map[string][]string{"body.n": {}}
	db.mocks[4].Spec.ReqBodyNoise = map[string][]string{"body.n": {}, "body.ts": {}}
	before := counts()
	if own := h.sameCallBodies(db.mocks, mocknoise.NewJSONBody([]byte(`{"q":"a","ts":7,"n":1}`)), nil); !slices.Equal(names(own), []string{"mock-search-1"}) {
		t.Fatalf("the call's own recordings are %q, want mock-search-1", names(own))
	}
	if c := since(before); c[1] != 4 || c[2] != 1 {
		t.Fatalf("the call was printed %d times and compared with %d recordings, want 4 (ts, none, n, n and ts) and 1", c[1], c[2])
	}
}

// A print says a recording is another call's only where both bodies are
// JSON and its print is decided.
func TestAPrintSaysNothingOfABodyThatIsNotJSON(t *testing.T) {
	setProven(t, false)
	rec := func(body string) (*models.Mock, *ownRead) {
		m := &models.Mock{Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Body: body}}}
		var o ownBodies
		r, _ := o.read(m)
		return m, r
	}
	for _, tc := range []struct{ name, recorded, call string }{
		{name: "the call's body", recorded: `{"a":1}`, call: `a=1`},
		{name: "the recording's body", recorded: `a=1`, call: `{"a":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, r := rec(tc.recorded)
			if newCallPrints(mocknoise.NewJSONBody([]byte(tc.call)), nil).another(m, r) {
				t.Fatalf("the print of %s says %s is another call's", tc.call, tc.recorded)
			}
		})
	}
}

// Two lists of other keys are never written alike, so a print taken under
// one noise is never read as one taken under another.
func TestKeysKeyWritesOtherKeysApart(t *testing.T) {
	for _, pair := range [][2][]string{
		{{"a", "bc"}, {"ab", "c"}},
		{{"body.a", "body.body.c"}, {"body.abody.", "body.c"}},
		{{""}, nil},
		{{"a", ""}, {"a"}},
	} {
		if keysKey(pair[0]) == keysKey(pair[1]) {
			t.Fatalf("%q and %q are written alike: %q", pair[0], pair[1], keysKey(pair[0]))
		}
	}
	if noiseKey(map[string][]string{"ts": {"^x$"}, "q": nil}) != keysKey([]string{"q", "ts"}) {
		t.Fatal("a noise's key is not its keys sorted, whatever their patterns")
	}
}

// A recording only printed keeps no read of its body: one whose print is
// not the call's is never read whole, so a pool of other calls' recordings
// keeps no decoded document alive. A recording whose print is the call's is
// read whole once to compare it, and kept so; its print is read from that
// read, never from another decode.
func TestARecordingOnlyPrintedKeepsNoReadOfItsBody(t *testing.T) {
	setProven(t, false)
	h := newHTTP()
	db := searchRecordings(t, h, nil, `{"q":"ada"}`, `{"q":"alan"}`, `{"q":"bob"}`)
	before := ownBodyReads.Load()
	if own := h.sameCallBodies(db.mocks, mocknoise.NewJSONBody([]byte(`{"q":"eve"}`)), nil); len(own) != 0 {
		t.Fatalf("the call's own recordings are %q, want none", names(own))
	}
	if n := ownBodyReads.Load() - before; n != 3 {
		t.Fatalf("the call read the recordings' bodies %d times, want 3 (each printed)", n)
	}
	kept := func() []string {
		var out []string
		for _, m := range db.mocks {
			if r, _ := h.ownBodies.read(m); r.whole.Load() != nil {
				out = append(out, m.Name)
			}
		}
		return out
	}
	if k := kept(); len(k) != 0 {
		t.Fatalf("recordings only printed keep a read of their bodies: %q", k)
	}
	before = ownBodyReads.Load()
	if own := h.sameCallBodies(db.mocks, mocknoise.NewJSONBody([]byte(`{ "q" : "alan" }`)), nil); !slices.Equal(names(own), []string{"mock-search-2"}) {
		t.Fatalf("the call's own recordings are %q, want mock-search-2", names(own))
	}
	if n := ownBodyReads.Load() - before; n != 1 {
		t.Fatalf("the call read the recordings' bodies %d times, want 1 (its own, read whole)", n)
	}
	if k := kept(); !slices.Equal(k, []string{"mock-search-2"}) {
		t.Fatalf("the recordings keeping a read of their bodies are %q, want mock-search-2", k)
	}
	r, _ := h.ownBodies.read(db.mocks[1])
	r.print.Store(nil)
	before = ownBodyReads.Load()
	if r.printOf(db.mocks[1], nil, "") == nil || ownBodyReads.Load() != before {
		t.Fatal("a recording read whole was decoded again to print it")
	}
}

// The report of a call served another call's recording renders that
// recording's request as renderMockRequest does, once for each recorded
// request however many calls it serves; a recording whose method, URL,
// header, body or redacted values are not what was rendered is rendered
// again.
func TestARecordingServedToManyCallsIsRenderedOnce(t *testing.T) {
	setProven(t, false)
	x := &models.Mock{Name: "x", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{
		Method: "POST", URL: "http://api.local/rendered-once", Header: map[string]string{"Content-Type": "application/json"}, Body: `{"q":"rendered-once"}`}}}
	before := rendersMade.Load()
	for i := 0; i < 3; i++ {
		if got, want := renderedOf(x), renderMockRequest(x.Spec.HTTPReq, x.Noise); got != want {
			t.Fatalf("the report renders %q, want %q", got, want)
		}
	}
	if n := rendersMade.Load() - before; n != 1 {
		t.Fatalf("three reports of one recording rendered it %d times, want once", n)
	}
	for _, change := range []struct {
		name string
		do   func()
	}{
		{"its method", func() { x.Spec.HTTPReq.Method = "PUT" }},
		{"its URL", func() { x.Spec.HTTPReq.URL = "http://api.local/find" }},
		{"a header", func() { x.Spec.HTTPReq.Header["X-Team"] = "a" }},
		{"its body", func() { x.Spec.HTTPReq.Body = `{"q":"rendered-once-again"}` }},
		{"its redacted values", func() { x.Noise = []string{"^rendered-once-again$"} }},
	} {
		change.do()
		before = rendersMade.Load()
		if got, want := renderedOf(x), renderMockRequest(x.Spec.HTTPReq, x.Noise); got != want || rendersMade.Load()-before != 1 {
			t.Fatalf("after a change to %s the report renders %q (%d renders), want %q rendered again", change.name, got, rendersMade.Load()-before, want)
		}
	}
}

// The rendered requests kept start again from none past maxKeptRenders.
func TestKeptRendersStartAgainPastTheirBound(t *testing.T) {
	var k keptRenders
	k.put(renderKey{method: "GET"}, "a")
	k.put(renderKey{method: "PUT"}, strings.Repeat("x", maxKeptRenders-1))
	if _, ok := k.get(renderKey{method: "GET"}); !ok || k.bytes != maxKeptRenders {
		t.Fatalf("renders up to the bound were not all kept: %d bytes", k.bytes)
	}
	k.put(renderKey{method: "POST"}, "b")
	if _, ok := k.get(renderKey{method: "GET"}); ok || len(k.of) != 1 || k.bytes != 1 {
		t.Fatalf("past the bound keptRenders keeps %d renders of %d bytes, want only the last", len(k.of), k.bytes)
	}
}
