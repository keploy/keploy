package http

import (
	"slices"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
)

// formRecordings records each of bodies as a form POST /search, answered
// {"hit":<i>} for the ith (from 1), named mock-search-<i>.
func formRecordings(t *testing.T, h *HTTP, bodies ...string) *mockMemDb {
	db := &mockMemDb{updateUnFilteredReturn: true}
	for i, b := range bodies {
		db.mocks = append(db.mocks, recordedAs(t, h, "mock-search-"+strconv.Itoa(i+1), i+1, formSearchCall(b), jsonAnswer(200, `{"hit":`+strconv.Itoa(i+1)+`}`), nil))
	}
	return db
}

// A form key the call or the one recording left holds, written in capitals
// or escaped (userId, ad%6Din), is named by its decoded name, as the strict
// comparison names it, and the entry the next step names for it, read as
// keploy reads keploy.yml (in lower case), covers it: with that noise the
// recording is served as the call's own, with no report, in each mode.
// Before, an entry with no patterns named a form key only as written, and
// keploy reads keploy.yml's request-body keys in lower case, so the call
// stayed reported closest_body whatever keploy.yml declared.
func TestAFormKeyIsNamedAndExcusedAsKeployYmlNamesIt(t *testing.T) {
	for _, tc := range []struct{ name, recorded, call, field string }{
		{name: "a key only the call has, named in capitals", recorded: "q=ada", call: "q=ada&userId=1", field: "body.userId"},
		{name: "a key only the call has, escaped in capitals", recorded: "q=ada", call: "q=ada&ad%6Din=1", field: "body.admin"},
		{name: "a key in capitals only the recording has", recorded: "q=ada&userId=1", call: "q=ada", field: "body.userId"},
		{name: "a key escaped in capitals only the recording has", recorded: "q=ada&ad%6Din=1", call: "q=ada", field: "body.admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setProven(t, false)
			h := newHTTP()
			got := serveReportedWith(t, h, oneRecording(t, h, formSearchCall, tc.recorded), models.OutgoingOptions{}, formSearchCall(tc.call))
			a := got.answers()
			if got.status != 200 || len(a) != 1 || a[0].Answer != models.AnswerClosestBody || !slices.Equal(a[0].Differs, []string{tc.field}) {
				t.Fatalf("%s against the one recording %s got %d with answer reports %+v, want it served and one %q naming %s", tc.call, tc.recorded, got.status, a, models.AnswerClosestBody, tc.field)
			}
			next := models.OutgoingOptions{NoiseConfig: keployYmlRequestBodyNoise(t, a[0].NextSteps)}
			for _, proven := range []bool{false, true} {
				setProven(t, proven)
				h := newHTTP()
				again := serveReportedWith(t, h, oneRecording(t, h, formSearchCall, tc.recorded), next, formSearchCall(tc.call))
				if again.status != 200 || again.body != `{"hit":1}` || len(again.answers()) != 0 {
					t.Fatalf("proven %v: with the next step's keploy.yml noise %v, %s got %d %q with answer reports %+v, want the recording served as its own", proven, next.NoiseConfig, tc.call, again.status, again.body, again.answers())
				}
			}
		})
	}
}

// Of several form recordings, the one whose body is the call's outside the
// request-body noise keploy.yml declares for a key written in capitals
// (userId, which keploy reads as userid) is the call's own: it is served
// with no report, in the default and the strict noise mode and under the
// proven switch. Before, the entry never named the key, so the call was
// served the closest and reported closest_body.
func TestOfSeveralFormRecordingsTheCallsOwnOutsideAKeyInCapitalsIsServed(t *testing.T) {
	noise := map[string]map[string][]string{"requestbody": {"userId": {}}}
	for _, tc := range []struct {
		name   string
		opts   models.OutgoingOptions
		proven bool
	}{
		{name: "default noise mode", opts: models.OutgoingOptions{NoiseConfig: noise}},
		{name: "strict noise mode", opts: models.OutgoingOptions{NoiseConfig: noise, MockNoiseStrict: true}},
		{name: "proven switch", opts: models.OutgoingOptions{NoiseConfig: noise}, proven: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setProven(t, tc.proven)
			h := newHTTP()
			got := serveReportedWith(t, h, formRecordings(t, h, "q=ada&userId=1", "q=bob&userId=2"), tc.opts, formSearchCall("q=ada&userId=3"))
			if got.status != 200 || got.body != `{"hit":1}` || len(got.answers()) != 0 {
				t.Fatalf("q=ada&userId=3 got %d %q with answer reports %+v, want q=ada&userId=1's answer and none", got.status, got.body, got.answers())
			}
		})
	}
}

// formReqBodyNoise skips a key an entry with no patterns names as written
// or by its decoded key as the strict comparison reads it (in any case,
// an id key by an entry naming id keys), and names a key's drift as written
// for the learn step (Diff) and by its decoded key for the call's own check
// (DiffBothWays). An entry with patterns still decides the key it names.
func TestFormReqBodyNoiseReadsAKeyAsTheStrictComparisonDoes(t *testing.T) {
	for _, tc := range []struct {
		name, rec, live string
		known           map[string][]string
		asWritten       []string // the drift Diff names
		byName          []string // the drift DiffBothWays names
	}{
		{name: "an escaped key only the recording has", rec: "q=ada&ad%6Din=1", live: "q=ada", asWritten: []string{"body.ad%6Din"}, byName: []string{"body.admin"}},
		{name: "a key in capitals known in lower case", rec: "q=ada&userId=1", live: "q=ada&userId=2", known: map[string][]string{"body.userid": {}}},
		{name: "a key in capitals only the recording has, known in lower case", rec: "q=ada&userId=1", live: "q=ada", known: map[string][]string{"body.userid": {}}},
		{name: "an escaped key known by its decoded name", rec: "ad%6Din=1", live: "ad%6Din=2", known: map[string][]string{"body.admin": {}}},
		{name: "an escaped key known as written", rec: "ad%6Din=1", live: "ad%6Din=2", known: map[string][]string{"body.ad%6Din": {}}},
		{name: "an id key known by an entry naming id keys", rec: "1234567890=1", live: "1234567890=2", known: map[string][]string{"body.{id}": {}}},
		{name: "a key whose entry has patterns", rec: "userId=1", live: "userId=x", known: map[string][]string{"body.userid": {"^[0-9]+$"}}, asWritten: []string{"body.userId"}, byName: []string{"body.userId"}},
		{name: "another key's entry", rec: "userId=1", live: "userId=2", known: map[string][]string{"body.user": {}}, asWritten: []string{"body.userId"}, byName: []string{"body.userId"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, by := range []struct {
				byName bool
				want   []string
			}{{false, tc.asWritten}, {true, tc.byName}} {
				var got []string
				for k := range formReqBodyNoise(tc.rec, tc.live, tc.known, nil, by.byName) {
					got = append(got, k)
				}
				slices.Sort(got)
				if !slices.Equal(got, by.want) {
					t.Errorf("byName %v: %s against %s drifts at %q, want %q", by.byName, tc.live, tc.rec, got, by.want)
				}
			}
		})
	}
	h := newHTTP()
	m := recordedAs(t, h, "f", 1, formSearchCall("q=ada&ad%6Din=1"), jsonAnswer(200, `{}`), nil)
	if d, _ := (httpNoiseAdapter{}).Diff(m, []byte("q=ada&ad%6Din=1"), []byte("q=ada"), nil, nil, false, nil); len(d) != 1 || d["body.ad%6Din"] == nil {
		t.Errorf("the learn step's form drift is %v, want body.ad%%6Din, as written", d)
	}
	if d, _ := (httpNoiseAdapter{}).DiffBothWays(m, mocknoise.NewJSONBody([]byte("q=ada&ad%6Din=1")), mocknoise.NewJSONBody([]byte("q=ada")), nil, nil, nil); len(d) != 1 || d["body.admin"] == nil {
		t.Errorf("the call's own check's form drift is %v, want body.admin, by its decoded name", d)
	}
}

// fieldsOnOneSide names each form key only one form holds by the side that
// holds it, by its decoded name, and a key both hold by none.
func TestFieldsOnOneSideNamesAFormKeyByItsSide(t *testing.T) {
	h := newHTTP()
	db := formRecordings(t, h, "q=ada&ad%6Din=1")
	recorded, _ := h.ownBodies.read(db.mocks[0])
	got := fieldsOnOneSide(db.mocks[0], recorded.json(), mocknoise.NewJSONBody([]byte("q=bob&userId=1")))
	want := map[string]sides{"body.admin": onlyRecorded, "body.userId": onlyLive}
	if len(got) != len(want) || got["body.admin"] != want["body.admin"] || got["body.userId"] != want["body.userId"] {
		t.Fatalf("fields on one side of the form q=bob&userId=1 against q=ada&ad%%6Din=1 = %v, want %v", got, want)
	}
}
