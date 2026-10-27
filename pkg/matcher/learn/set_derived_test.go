package learn

import (
	"fmt"
	"net/http"
	"reflect"
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
		if got, want := dc.keyedPlaces(x), own.keyedPlaces(x); !reflect.DeepEqual(got, want) {
			t.Errorf("%s at %s: the set's keyed places %v, want the recording's own %v", m.Name, m.Spec.ReqTimestampMock, got, want)
		}
		if got, want := dc.payloadMoments(x), own.payloadMoments(x); !reflect.DeepEqual(got, want) {
			t.Errorf("%s at %s: the set's instants %v, want the recording's own %v", m.Name, m.Spec.ReqTimestampMock, got, want)
		}
	}
}
