package learn

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A test's drift cache starts empty, so each test of a set read the instants,
// keyed places and words of every payload served or recorded before its calls
// again, and the learn step's cost per call grew with the set (api-server's
// full round: spansOf, keyedPlaces, searchedTokens and rawExtra under
// ClassifyFollowing, up to 6.8 s of a 30 s profile window). What is read of a
// recording's payload alone is now kept for the set by the recording, never
// by its text: the second test of a set reads none of the payloads the first
// read, learns what the first learned, and what a cache of no tracker learns;
// the next set reads them again.
func TestASetsSecondTestReadsNoEarlierPayloadAgain(t *testing.T) {
	const n = 40
	at := eRecAt
	recorded := func(i int) *models.Mock {
		stamp := at.Add(-time.Duration(n-i) * time.Minute).Format(time.RFC3339Nano)
		return &models.Mock{Name: fmt.Sprintf("mock-%d", i), Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: at.Add(-time.Duration(n-i) * time.Second),
			ResTimestampMock: at.Add(-time.Duration(n-i)*time.Second + time.Millisecond),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: fmt.Sprintf("SELECT id, updated_at FROM filler%d", i),
				Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: fmt.Sprintf("filler%d", i)}, {Value: stamp}}}, CommandComplete: "SELECT 1"},
			}},
		}}
	}
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		mocks[i] = recorded(i)
	}
	// Each test reads the recordings anew, as the agent's learn step does
	// (a Raw for each call, from the store's mocks).
	rawOf := func() []Raw {
		raw := make([]Raw, n)
		for i, m := range mocks {
			raw[i] = RawOf(m, true).ServedAs(i + 1).ServedAsRecorded()
		}
		return raw
	}
	// A read of the clock, which the offset rule reads every earlier
	// payload's instants for, and a word that changed, which the word rule
	// looks for in every earlier payload's words.
	const clock, word = "Req.query.bind[0]", "Req.query.bind[1]"
	rec := Shape{Leaves: map[string]Leaf{clock: {Path: clock, Type: String, Value: "2026-10-24T09:55:53.9Z"}, word: {Path: word, Type: String, Value: "a1b2c3d4"}}}
	live := Shape{Leaves: map[string]Leaf{clock: {Path: clock, Type: String, Value: "2026-10-24T09:56:54.1Z"}, word: {Path: word, Type: String, Value: "z9y8x7w6"}}}
	classify := func(dc *DriftCache) []Learned {
		dc.appClock = true
		return ClassifyFollowing(dc, []string{clock, word}, live, rec, at, nil, rawOf(), nil, nil, nil)
	}
	// A payload is read where its instants or its words are: every earlier
	// payload, and each of its fields, holds a "filler" word.
	spans, words := instantSpans, tokensOfForms
	defer func() { instantSpans, tokensOfForms = spans, words }()
	read := 0
	instantSpans = func(s string) []instant {
		if strings.Contains(s, "filler") {
			read++
		}
		return spans(s)
	}
	tokensOfForms = func(forms []string) []map[string]bool {
		if strings.Contains(strings.Join(forms, "\n"), "filler") {
			read++
		}
		return words(forms)
	}

	want := classify(NewDriftCache())
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	read = 0
	first := classify(tr.Drifts())
	if read == 0 {
		t.Fatalf("the first test read no payload: the row reads nothing")
	}
	tr.End()
	tr.Begin()
	read = 0
	second := classify(tr.Drifts())
	if read != 0 {
		t.Errorf("the second test of the set read the instants or words of payloads %d times again, want 0: what is read of a recording's payload alone is kept for the set", read)
	}
	tr.End()
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Errorf("learned %v in the first test and %v in the second, want %v (a cache of no tracker's)", first, second, want)
	}

	tr.NewSet()
	tr.Begin()
	read = 0
	if again := classify(tr.Drifts()); !reflect.DeepEqual(again, want) {
		t.Errorf("learned %v in the next set, want %v", again, want)
	}
	if read == 0 {
		t.Errorf("the next set read no payload: a recording staged again for another set must be read again")
	}
}

// What the set keeps of a payload is that recording's, as RawOf read it: a
// recording of another name's time (names restart in each recorded test set)
// and one whose answer the agent parked on disk, read without it, are each
// read on their own, and an answer's stamps are blanked or kept as each read
// asks. Each read gives what a cache of no tracker gives.
func TestASetKeepsEachRecordingsOwnReads(t *testing.T) {
	at := eRecAt.Truncate(time.Second)
	httpMock := func(name string, asked time.Time, body string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			ReqTimestampMock: asked, ResTimestampMock: asked.Add(time.Second),
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://api/v1/jobs"},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Date": asked.Format(http.TimeFormat)}, Body: body},
		}}
	}
	first := httpMock("mock-1", at.Add(-time.Hour), `{"id":"j1","updated_at":"2026-10-24T08:55:53Z"}`)
	other := httpMock("mock-1", at.Add(-time.Minute), `{"id":"j2","created_at":"2026-10-24T09:54:53.5Z","n":"filler"}`)
	parked := httpMock("mock-1", at.Add(-time.Hour), "")
	parked.Spec.HTTPResp.Body, parked.Spec.HTTPResp.Header = "", nil
	parked.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) { return first.Spec.HTTPResp, nil, nil })
	if !parked.HasSpilledResponse() {
		t.Fatal("the parked recording holds its answer")
	}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	dc := tr.Drifts()
	for _, m := range []*models.Mock{first, other, parked} {
		x := RawOf(m, true)
		own := NewDriftCache()
		for _, keep := range []bool{false, true} {
			if got, want := dc.searchedTokens(x, keep), own.searchedTokens(x, keep); !reflect.DeepEqual(got, want) {
				t.Errorf("%s at %s, stamps kept %v: the set's words differ from the recording's own", m.Name, m.Spec.ReqTimestampMock, keep)
			}
		}
		for _, keep := range []bool{false, true} {
			want := map[string]bool{}
			for _, text := range own.searched(x, keep) {
				for w := range tokensBy(text, isWord) {
					want[w] = true
				}
			}
			if got, ok := dc.searchWords(x, keep); ok && !reflect.DeepEqual(got, want) {
				t.Errorf("%s at %s, stamps kept %v: the set's search words differ from the recording's own", m.Name, m.Spec.ReqTimestampMock, keep)
			}
		}
		if got, want := dc.keyedPlaces(x), own.keyedPlaces(x); !reflect.DeepEqual(got, want) {
			t.Errorf("%s at %s: the set's keyed places %v, want the recording's own %v", m.Name, m.Spec.ReqTimestampMock, got, want)
		}
		if got, want := dc.payloadMoments(x), own.payloadMoments(x); !reflect.DeepEqual(got, want) {
			t.Errorf("%s at %s: the set's instants %v, want the recording's own %v", m.Name, m.Spec.ReqTimestampMock, got, want)
		}
	}
}

// fillers are n recordings of the set before at, each a row of its own
// "filler" word and number and no instant: payloads that hold no value a
// row searches for.
func fillers(n int, at time.Time) []*models.Mock {
	out := make([]*models.Mock, n)
	for i := range out {
		out[i] = &models.Mock{Name: fmt.Sprintf("filler-%d", i), Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: at.Add(-time.Duration(n-i) * time.Second),
			ResTimestampMock: at.Add(-time.Duration(n-i)*time.Second + time.Millisecond),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: fmt.Sprintf("SELECT name, n FROM filler%d", i),
				Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: fmt.Sprintf("filler%d", i)}, {Value: fmt.Sprint(7000 + i)}}}, CommandComplete: "SELECT 1"},
			}},
		}}
	}
	return out
}

// The word rule searched the text of every payload served or recorded
// before a call for each word of a changed value, once per call, so its cost
// per call grew with the set (appearedIn: searched and ContainsToken). A
// payload is now read only where its words, read once for the set, hold the
// runs of letters and digits of a value searched for (mayHold): a payload
// that holds no searched value's word is read by no call, in no test of the
// set, and each call learns what a cache of no tracker learns.
func TestTheWordSearchReadsNoPayloadWithoutTheValuesWords(t *testing.T) {
	at := eRecAt
	mocks := fillers(40, at)
	rawOf := func() []Raw {
		raw := make([]Raw, len(mocks))
		for i, m := range mocks {
			raw[i] = RawOf(m, true).ServedAs(i + 1).ServedAsRecorded()
		}
		return raw
	}
	// A word that changed, searched in every earlier payload: whole, and by
	// its words, one of more than one run ("a1b2-c3d4").
	const word, two = "Req.query.bind[0]", "Req.query.bind[1]"
	rec := Shape{Leaves: map[string]Leaf{word: {Path: word, Type: String, Value: "a1b2c3d4"}, two: {Path: two, Type: String, Value: "q7-r8s9"}}}
	live := Shape{Leaves: map[string]Leaf{word: {Path: word, Type: String, Value: "z9y8x7w6"}, two: {Path: two, Type: String, Value: "q7-k5m4"}}}
	classify := func(dc *DriftCache) []Learned {
		return ClassifyFollowing(dc, []string{word, two}, live, rec, at, nil, rawOf(), nil, nil, nil)
	}
	search := tokenIndex
	defer func() { tokenIndex = search }()
	read := 0
	tokenIndex = func(s, v string) int {
		if strings.Contains(s, "filler") {
			read++
		}
		return search(s, v)
	}

	read = 0
	want := classify(NewDriftCache())
	if read == 0 {
		t.Fatalf("a cache of no tracker searched no payload's text: the row searches nothing")
	}
	tr := NewTracker()
	tr.NewSet()
	for test := 1; test <= 2; test++ {
		tr.Begin()
		for call := 1; call <= 2; call++ {
			read = 0
			got := classify(tr.Drifts())
			if read != 0 {
				t.Errorf("test %d, call %d: the word rule searched the text of payloads holding none of the value's words %d times, want 0", test, call, read)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("test %d, call %d: learned %v, want %v (a cache of no tracker's)", test, call, got, want)
			}
		}
		tr.End()
	}
}

// The echo read the instants of each field of every payload served or
// recorded before a call (echoRaw: holdsRecorded of each field's text) anew
// in each test of a set. They are now read once for the set, by the
// recording: the second test of a set reads no field of a payload the first
// read, and finds the echo's source where a cache of no tracker does.
func TestASetsSecondTestReadsNoEarlierFieldsInstantsAgain(t *testing.T) {
	sent := time.Date(2026, 10, 22, 11, 14, 16, 473e6, time.UTC)
	got := sent.Add(2*time.Minute + 311*time.Millisecond)
	attrs := func(at time.Time) string { return `{"SentTimestamp": "` + strconv.FormatInt(at.UnixMilli(), 10) + `"}` }
	// The receive holds the moment in a field (its SentTimestamp), or only
	// in its payload's other forms (a note's base64 text).
	encoded := func(at time.Time) string {
		return `{"Note": "` + base64.StdEncoding.EncodeToString([]byte("sent at "+strconv.FormatInt(at.UnixMilli(), 10)+" by the producer")) + `"}`
	}
	for _, inField := range []bool{true, false} {
		receive, delivered := r1Receive(encoded(sent)), r1Receive(encoded(got))
		if inField {
			receive, delivered = r1Receive(attrs(sent)), r1Receive(attrs(got))
		}
		delivered.Name = "mock-11"
		// Before it, a recording whose field holds the moment only as a
		// time-ordered UUID's time, which names no place a value came from.
		ms := sent.UnixMilli()
		ref := &models.Mock{Name: "uuid-ref", Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: r1Insert.Add(-time.Hour),
			ResTimestampMock: r1Insert.Add(-time.Hour + time.Millisecond),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: "SELECT ref FROM refs",
				Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: fmt.Sprintf("%08x-%04x-7abc-8def-0123456789ab", ms>>16, ms&0xffff)}}}, CommandComplete: "SELECT 1"},
			}},
		}}
		echoesAcrossTests(t, inField, append(append(fillers(40, r1Insert), ref), receive, delivered), sent, got)
	}
}

// echoesAcrossTests is TestASetsSecondTestReadsNoEarlierFieldsInstantsAgain
// of one receive.
func echoesAcrossTests(t *testing.T, inField bool, mocks []*models.Mock, sent, got time.Time) {
	t.Helper()
	const p = "body.docs.msgs.0.processed_at.$date.$numberLong"
	classify := func(dc *DriftCache) Learned {
		dc.appClock = true
		var raw []Raw
		for _, m := range mocks {
			raw = append(raw, RawOf(m, true))
		}
		return ClassifyIn(dc, []string{p}, processedAt(got, false), processedAt(sent, false), r1Insert, nil, raw, nil, nil)[0]
	}
	want := classify(NewDriftCache())
	if want.Kind != KindVariable || want.From.Mock != "mock-4" || !want.From.Echo || (want.From.Field != "") != inField {
		t.Fatalf("in a field %v: learned %s from %q at %q (echo %v) with a cache of no tracker, want the echo of mock-4: the row reads no field of the fillers", inField, want.Kind, want.From.Mock, want.From.Field, want.From.Echo)
	}
	spans := instantSpans
	defer func() { instantSpans = spans }()
	read := 0
	instantSpans = func(s string) []instant {
		if strings.Contains(s, "filler") {
			read++
		}
		return spans(s)
	}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	read = 0
	if first := classify(tr.Drifts()); !reflect.DeepEqual(first, want) {
		t.Errorf("in a field %v: learned %v in the first test, want %v", inField, first, want)
	}
	if read == 0 {
		t.Fatalf("in a field %v: the first test read no filler's instants: the row reads nothing", inField)
	}
	tr.End()
	tr.Begin()
	read = 0
	if second := classify(tr.Drifts()); !reflect.DeepEqual(second, want) {
		t.Errorf("in a field %v: learned %v in the second test, want %v", inField, second, want)
	}
	if read != 0 {
		t.Errorf("in a field %v: the second test of the set read the instants of earlier payloads' fields %d times again, want 0", inField, read)
	}
	tr.End()
}

// The word rule's search, which reads a payload only where its words may
// hold a value searched for (mayHold), finds each value where reading every
// payload's text finds it: a value of one word, a value of more than one run
// ("q7-r8s9") held whole and with its runs apart (found by its part), one
// held only in another form of the payload (base64), and a whole clock read.
// Each test of a set learns what a cache of no tracker learns, from the
// payload that holds it.
func TestTheWordSearchFindsWhatReadingEveryPayloadFinds(t *testing.T) {
	at := eRecAt
	const p = "Req.query.bind[0]"
	for _, c := range []struct {
		name, rec, live, holds string
		found                  bool
	}{
		{"one word", "a1b2c3d4", "z9y8x7w6", "id a1b2c3d4 kept", true},
		{"runs whole", "q7-r8s9", "k5-m4n3", "ref q7-r8s9 kept", true},
		{"runs apart", "q7-r8s9", "k5-m4n3", "ref q7 then r8s9", true},
		{"in base64", "a1b2c3d4", "z9y8x7w6", "id " + base64.StdEncoding.EncodeToString([]byte("token a1b2c3d4 kept")), true},
	} {
		src := &models.Mock{Name: "src", Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: at.Add(-time.Second / 2),
			ResTimestampMock: at.Add(-time.Second/2 + time.Millisecond),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: "SELECT note FROM src",
				Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: c.holds}}}, CommandComplete: "SELECT 1"},
			}},
		}}
		mocks := append(fillers(10, at.Add(-time.Second)), src)
		rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: c.rec}}}
		live := Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: c.live}}}
		classify := func(dc *DriftCache) []Learned {
			raw := make([]Raw, len(mocks))
			for i, m := range mocks {
				raw[i] = RawOf(m, true).ServedAs(i + 1).ServedAsRecorded()
			}
			dc.appClock = true
			return ClassifyFollowing(dc, []string{p}, live, rec, at, nil, raw, nil, nil, nil)
		}
		want := classify(NewDriftCache())
		if found := len(want) == 1 && (want[0].From.Mock == "src" || strings.Contains(want[0].Why, " in src ")); found != c.found {
			t.Errorf("%s: a cache of no tracker learned %v, want the value found in src %v", c.name, want, c.found)
		}
		tr := NewTracker()
		tr.NewSet()
		for test := 1; test <= 2; test++ {
			tr.Begin()
			if got := classify(tr.Drifts()); !reflect.DeepEqual(got, want) {
				t.Errorf("%s, test %d: learned %v, want %v (a cache of no tracker's)", c.name, test, got, want)
			}
			tr.End()
		}
	}
	// A whole clock read, searched whole (containsWholeClock) where its
	// payload's words may hold it.
	clock := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	holds := &models.Mock{Name: "src", Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: clock.Add(-time.Second),
		ResTimestampMock: clock.Add(-time.Second + time.Millisecond),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "SELECT id, at FROM src",
			Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: "42"}, {Value: "2026-10-14 10:00:41"}}}, CommandComplete: "SELECT 1"},
		}},
	}}
	mocks := append(fillers(10, clock.Add(-2*time.Second)), holds)
	// Sent as is, and percent-escaped: the payload holds the escaped read
	// only as it decodes ("2010" is no word of it).
	for _, read := range [][2]string{{"2026-10-14 10:00:41", "2026-10-14 10:01:42"}, {"2026-10-14%2010:00:41", "2026-10-14%2010:01:42"}} {
		find := func(dc *DriftCache) (Place, bool) {
			var raw []Raw
			for _, m := range mocks {
				raw = append(raw, RawOf(m, true))
			}
			dc.appClock = true
			return appeared(dc, read[0], read[1], clock, nil, raw)
		}
		want, ok := find(NewDriftCache())
		if !ok || want.Mock != "src" {
			t.Fatalf("a whole clock read %q: a cache of no tracker found %v at %q, want src", read[0], ok, want.Mock)
		}
		tr := NewTracker()
		tr.NewSet()
		for test := 1; test <= 2; test++ {
			tr.Begin()
			if got, ok := find(tr.Drifts()); !ok || !reflect.DeepEqual(got, want) {
				t.Errorf("a whole clock read %q, test %d: found %v at %v, want %v", read[0], test, ok, got, want)
			}
			tr.End()
		}
	}
}
