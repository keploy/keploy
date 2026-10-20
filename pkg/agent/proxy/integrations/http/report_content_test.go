package http

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
	"go.keploy.io/server/v3/utils/logcontent"
	"gopkg.in/yaml.v3"
)

// piiMiss is the miss report of a call whose body, query and headers carry
// an application's values under names keploy's sensitive-key redaction does
// not know (an email, an SSN, a customer id), against a recording of the
// same call with other values.
func piiMiss(t *testing.T) *models.MockMismatchReport {
	t.Helper()
	h := newHTTP()
	mock := httpMockWithReq("mock-1", "POST", "http://users.internal:8080/api/users?customer=kp-rec-cust",
		`{"user":"alice","email":"kp-rec@example.com","ssn":"kp-rec-ssn"}`,
		map[string]string{"Content-Type": "application/json", "X-Customer": "kp-rec-header"}, nil)
	db := &mockMemDb{mocks: []*models.Mock{mock}}
	req := makeReqWithQuery("POST", "/api/users", "customer=kp-live-cust")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Customer", "kp-live-header")
	live := []byte(`{"user":"alice","email":"kp-live@example.com","ssn":"kp-live-ssn"}`)
	diag := &matchDiag{phase: models.MatchPhaseBody, candidates: 1, schemaMatched: []*models.Mock{mock}}
	return h.buildHTTPMismatchReport(req, live, db, nil, nil, nil, diag)
}

var piiValues = []string{"kp-rec-cust", "kp-live-cust", "kp-rec@example.com", "kp-live@example.com", "kp-rec-ssn", "kp-live-ssn", "kp-rec-header", "kp-live-header"}

// reportText is everything of r a report file, a printer or the platform
// reads.
func reportText(t *testing.T, r *models.MockMismatchReport) string {
	t.Helper()
	b, err := yaml.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// By default an HTTP miss's report names the fields the call differs in and
// none of the values it or the recording held: not in the field diffs, the
// rendered diff, the whole-request renders or the advice.
func TestAnHTTPMissReportCarriesNoContentByDefault(t *testing.T) {
	r := piiMiss(t)
	text := reportText(t, r)
	for _, v := range piiValues {
		if strings.Contains(text, v) {
			t.Errorf("the default report carries %q:\n%s", v, text)
		}
	}
	paths := map[string]models.MockFieldDiffKind{}
	for _, d := range r.FieldDiffs {
		paths[d.Path] = d.Kind
	}
	for _, p := range []string{"query.customer", "body.email", "body.ssn"} {
		if paths[p] != models.DiffKindValueChanged {
			t.Errorf("the default report does not name %s as changed: %+v", p, r.FieldDiffs)
		}
		if !strings.Contains(r.Diff, p+": value changed") {
			t.Errorf("the diff %q does not name %s", r.Diff, p)
		}
	}
	if !r.ContentWithheld || r.ClosestMockReq != "" || r.ReceivedReq != "" {
		t.Errorf("the default report keeps its renders or does not say it withheld them: %+v", r)
	}
}

// With the switch on the report carries the values, as it always did.
func TestAnHTTPMissReportCarriesTheContentWithTheSwitchOn(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	r := piiMiss(t)
	text := reportText(t, r)
	for _, v := range piiValues {
		if v == "kp-rec-header" || v == "kp-live-header" {
			continue // header values are never diffed, and the renders show the recorded one
		}
		if !strings.Contains(text, v) {
			t.Errorf("with the switch on the report lost %q:\n%s", v, text)
		}
	}
	if r.ContentWithheld || r.ClosestMockReq == "" || r.ReceivedReq == "" {
		t.Errorf("with the switch on the report is %+v", r)
	}
}

// pathMiss is the default report of a GET of /api/users/kp-live-id whose
// closest recording is of /api/users/kp-rec-id on the same host: through the
// strict comparison under the proven switch, or the request matcher without
// it. Each finds the path differs and says so in a field diff of the whole
// path.
func pathMiss(t *testing.T, proven bool) *models.MockMismatchReport {
	t.Helper()
	live := func() *http.Request {
		r := makeReq("GET", "/api/users/kp-live-id")
		r.Host = "users.internal:8080"
		r.Header = http.Header{"Accept": []string{"application/json"}}
		return r
	}
	rec := onHost("rec", "/api/users/kp-rec-id", "users.internal:8080")
	db := &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}
	diag := &matchDiag{phase: models.MatchPhaseExhausted, candidates: 1}
	if proven {
		t.Setenv(provenswitch.Env, "1")
		ok, _, d, err := newHTTP().match(context.Background(), getTo("/api/users/kp-live-id", "users.internal:8080"), db, nil, nil, nil, true, false, false, true, true)
		if err != nil || ok || d == nil || d.closest == nil {
			t.Fatalf("the strict comparison: ok %v, err %v, diag %+v", ok, err, d)
		}
		diag = d
	}
	return newHTTP().buildHTTPMismatchReport(live(), nil, db, nil, nil, nil, diag)
}

// A miss whose path is not its closest recording's names the path as what
// differs, and by default neither path: the field diff loses its values, and
// the summary, which names the call's path, loses the segment that differs
// and keeps the ones the recording has too. Under the proven switch or not.
// With the switch on the summary is the call's path, as it always was.
func TestAMissOnAnotherPathNamesNoSegmentThatDiffersByDefault(t *testing.T) {
	for _, proven := range []bool{false, true} {
		r := pathMiss(t, proven)
		text := reportText(t, r)
		for _, v := range []string{"kp-live-id", "kp-rec-id"} {
			if strings.Contains(text, v) {
				t.Errorf("proven %v: the default report carries %q:\n%s", proven, v, text)
			}
		}
		if r.ActualSummary != "GET /api/users/{}" {
			t.Errorf("proven %v: the summary is %q, want GET /api/users/{}", proven, r.ActualSummary)
		}
		if !strings.Contains(r.Diff, "path: value changed") || !r.ContentWithheld {
			t.Errorf("proven %v: the report does not say the path differs: %q, %+v", proven, r.Diff, r.FieldDiffs)
		}
	}
	t.Cleanup(logcontent.Set(true))
	for _, proven := range []bool{false, true} {
		if r := pathMiss(t, proven); r.ActualSummary != "GET /api/users/kp-live-id" || r.ContentWithheld {
			t.Errorf("proven %v, switch on: the summary is %q", proven, r.ActualSummary)
		}
	}
}

// A miss on a call another test recorded renders that recording's request, as
// the builder renders one: withheld by default.
func TestANotOwnedMissRendersNoRecordingByDefault(t *testing.T) {
	m := onHost("other-test", "/api/users", "localhost:50001")
	m.Spec.HTTPReq.Body = `{"email":"kp-rec@example.com"}`
	r := &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /api/users", ContentWithheld: true}
	markNotOwned(r, m)
	if r.ClosestMockReq != "" || !r.NotOwned || r.ClosestMock != "other-test" {
		t.Errorf("the not-owned report is %+v", r)
	}
	t.Cleanup(logcontent.Set(true))
	r = &models.MockMismatchReport{Protocol: "HTTP"}
	markNotOwned(r, m)
	if !strings.Contains(r.ClosestMockReq, "kp-rec@example.com") || r.ContentWithheld {
		t.Errorf("with the switch on the not-owned report renders %q", r.ClosestMockReq)
	}
}

// A test variable's miss names the variable, its place and where the test
// bound it, and by default neither the value it holds nor the one the call
// sent.
func TestAVariableMissNamesNoValueByDefault(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: createAndReadBack("ord-rec-1")}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"ord-live-9","item":"pen"}`)); !ok {
		t.Fatal("the create should match")
	}
	ok, _, diag := matchVar(t, h, db, liveReq("GET", "/orders/ord-other-3", ""))
	wantProblem(t, ok, diag, "this call sent")
	rep := h.buildHTTPMismatchReport(makeReq("GET", "/orders/ord-other-3"), nil, db, nil, nil, nil, diag)
	text := reportText(t, rep)
	for _, v := range []string{"ord-live-9", "ord-rec-1"} {
		if strings.Contains(text, v) {
			t.Errorf("the variable's miss carries %q:\n%s", v, text)
		}
	}
	for _, want := range []string{"orderId", "mock-1 body.id", "url.path[1]"} {
		if !strings.Contains(rep.Diff, want) {
			t.Errorf("the variable's diff %q does not name %q", rep.Diff, want)
		}
	}
	// Both values withheld: the variable's, and the one the call sent (which
	// its path holds, and the summary names as the path).
	if got := strings.Count(rep.Diff, models.WithheldValue); got != 2 {
		t.Errorf("the variable's diff %q withholds %d values, want 2", rep.Diff, got)
	}
	if len(rep.FieldDiffs) != 1 || rep.FieldDiffs[0].Path != "url.path[1]" || rep.FieldDiffs[0].Kind != models.DiffKindVariable {
		t.Errorf("field diffs %+v", rep.FieldDiffs)
	}
}

// Every miss of a test variable says what did not hold, and by default none
// of the values: not the call's, not the recording's, not the test's.
func TestEveryVariableMissNamesNoValueByDefault(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	headerAnswer := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"kp-v-rec"}`, 201, `{}`,
		models.MockVariables{"id": bindV("kp-v-rec", "body.id", "header.X-Order")}), 1)
	headerAnswer.Spec.HTTPResp.Header["X-Order"] = "kp-v-other"
	for _, tc := range []struct {
		name   string
		mocks  []*models.Mock
		given  []variables.Binding
		calls  []*req
		why    string
		values []string
	}{
		{
			name: "a value of another JSON type",
			mocks: []*models.Mock{at(varMock("mock-1", "POST", "http://store/orders", `{"id":"kp-v-rec","item":"pen"}`, 201, `{}`,
				models.MockVariables{"orderId": bindV("kp-v-rec", "body.id")}), 1)},
			calls:  []*req{liveReq("POST", "/orders", `{"id":31415926,"item":"pen"}`)},
			why:    "another JSON type",
			values: []string{"kp-v-rec", "31415926"},
		},
		{
			name: "another variable's value",
			mocks: []*models.Mock{
				at(varMock("mock-1", "POST", "http://auth/login", `{"user":"kp-v-rec-user"}`, 200, `{}`, models.MockVariables{"userId": bindV("kp-v-rec-user", "body.user")}), 1),
				at(varMock("mock-2", "POST", "http://store/orders", `{"id":"kp-v-rec-order"}`, 201, `{}`, models.MockVariables{"orderId": bindV("kp-v-rec-order", "body.id")}), 2),
			},
			calls:  []*req{liveReq("POST", "/login", `{"user":"kp-v-live"}`), liveReq("POST", "/orders", `{"id":"kp-v-live"}`)},
			why:    "which is variable userId's value",
			values: []string{"kp-v-live", "kp-v-rec-user", "kp-v-rec-order"},
		},
		{
			name: "a recorded request that does not hold the variable's value",
			mocks: []*models.Mock{at(varMock("mock-1", "POST", "http://store/orders", `{"id":"kp-v-body"}`, 201, `{}`,
				models.MockVariables{"orderId": bindV("kp-v-decl", "body.id")}), 1)},
			calls:  []*req{liveReq("POST", "/orders", `{"id":"kp-v-live"}`)},
			why:    "not variable orderId's recorded value",
			values: []string{"kp-v-body", "kp-v-decl", "kp-v-live"},
		},
		{
			name: "a body answer place that does not hold the recorded value",
			mocks: []*models.Mock{at(varMock("mock-1", "POST", "http://store/orders", `{"id":"kp-v-rec"}`, 201, `{"n":"kp-v-other"}`,
				models.MockVariables{"id": bindV("kp-v-rec", "body.id", "body.n")}), 1)},
			calls:  []*req{liveReq("POST", "/orders", `{"id":"kp-v-live"}`)},
			why:    "cannot hold variable id",
			values: []string{"kp-v-rec", "kp-v-other", "kp-v-live"},
		},
		{
			name: "a value that cannot stand at a body answer place",
			mocks: []*models.Mock{at(varMock("mock-1", "POST", "http://store/orders", `{"id":"271828"}`, 201, `{"n":271828}`,
				models.MockVariables{"id": bindV("271828", "body.id", "body.n")}), 1)},
			calls:  []*req{liveReq("POST", "/orders", `{"id":"kp-v-word"}`)},
			why:    "cannot stand at a place",
			values: []string{"271828", "kp-v-word"},
		},
		{
			name:   "a header answer place that does not hold the recorded value",
			mocks:  []*models.Mock{headerAnswer},
			calls:  []*req{liveReq("POST", "/orders", `{"id":"kp-v-live"}`)},
			why:    "does not hold variable id's recorded value",
			values: []string{"kp-v-rec", "kp-v-other", "kp-v-live"},
		},
		{
			name: "a template key recorded with another value",
			mocks: []*models.Mock{at(varMock("mock-1", "GET", "http://store/users/kpvrecmock", "", 200, `{}`,
				models.MockVariables{"id": useV("kpvrecmock", []string{"url.path[1]"})}), 1)},
			given:  []variables.Binding{{Name: "id", Value: "kpvlive", Recorded: "kpvrecgiven", Mock: "test case test-2", Place: "request"}},
			calls:  []*req{liveReq("GET", "/users/kpvlive", "")},
			why:    "the learn step and the templates disagree",
			values: []string{"kpvrecmock", "kpvrecgiven"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHTTP()
			db := &varDb{perTest: tc.mocks}
			if tc.given != nil {
				db.table.Start(tc.given)
			}
			var ok bool
			var diag *matchDiag
			for _, c := range tc.calls {
				ok, _, diag = matchVar(t, h, db, c)
			}
			p := wantProblem(t, ok, diag, tc.why)
			for _, v := range tc.values {
				if strings.Contains(p.why, v) || strings.Contains(p.diff(), v) {
					t.Errorf("the variable's evidence carries %q: %q", v, p.why)
				}
			}
			if !strings.Contains(p.why, models.WithheldValue) {
				t.Errorf("the variable's evidence %q does not say a value was withheld", p.why)
			}
		})
	}
}

// lifetimeMiss is pathMiss's call, with the proven switch off, against a
// recording that states its HTTP mocks' lifetimes, as every recording today's
// recorder makes does: its next steps say how to serve the call to every test
// (sessionCallHint), naming the call's path.
func lifetimeMiss(t *testing.T) *models.MockMismatchReport {
	t.Helper()
	t.Setenv(provenswitch.Env, "0")
	live := makeReq("GET", "/api/users/kp-live-id")
	live.Host = "users.internal:8080"
	rec := onHost("rec", "/api/users/kp-rec-id", "users.internal:8080")
	rec.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	db := &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}
	return newHTTP().buildHTTPMismatchReport(live, nil, db, nil, nil, nil, &matchDiag{phase: models.MatchPhaseExhausted, candidates: 1})
}

// The advice to declare a call shared names its path as the summary does: by
// default with the segment the summary withholds written {} there too, and
// said to be the call's own to write, so no door that keeps the advice (the
// agent's, the MOCK MISMATCH table's, keploy report's) names the value the
// summary withholds. With the switch on it names the call's path, as it did.
func TestASessionCallHintNamesThePathAsTheSummaryDoes(t *testing.T) {
	r := lifetimeMiss(t)
	if r.ActualSummary != "GET /api/users/{}" || !r.ContentWithheld {
		t.Fatalf("the summary is %q (withheld %v), want GET /api/users/{}", r.ActualSummary, r.ContentWithheld)
	}
	if want := `pathPrefix: "/api/users/{}"}], each {} the call's own segment there, which this report withholds)`; !strings.Contains(r.NextSteps, want) {
		t.Errorf("the hint does not name the path as the summary does (%s):\n%s", want, r.NextSteps)
	}
	door := models.UnmatchedCall{Protocol: r.Protocol, ActualSummary: r.ActualSummary, Diff: r.Diff, NextSteps: r.NextSteps,
		FieldDiffs: r.FieldDiffs, ClosestMockReq: r.ClosestMockReq, ReceivedReq: r.ReceivedReq, ContentWithheld: r.ContentWithheld}.ReportContent()
	for _, text := range []string{reportText(t, r), door.ActualSummary + "\n" + door.Diff + "\n" + door.NextSteps} {
		for _, v := range []string{"kp-live-id", "kp-rec-id"} {
			if strings.Contains(text, v) {
				t.Errorf("the default report carries %q:\n%s", v, text)
			}
		}
	}
	// A call on its closest recording's path, which differs elsewhere (a
	// body value): the summary withholds no segment, so the hint names the
	// path as it is and says nothing of a {}.
	t.Setenv(provenswitch.Env, "0")
	same := httpMockWithReq("rec", "POST", "http://users.internal:8080/api/users", `{"email":"kp-rec@example.com"}`,
		map[string]string{"Content-Type": "application/json"}, nil)
	same.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	body := makeReqWithQuery("POST", "/api/users", "")
	body.Header.Set("Content-Type", "application/json")
	r = newHTTP().buildHTTPMismatchReport(body, []byte(`{"email":"kp-live@example.com"}`), &mockMemDb{mocks: []*models.Mock{same}}, nil, nil, nil,
		&matchDiag{phase: models.MatchPhaseBody, candidates: 1, schemaMatched: []*models.Mock{same}})
	if want := `pathPrefix: "/api/users"}]), or record again`; !strings.Contains(r.NextSteps, want) || r.ActualSummary != "POST /api/users" {
		t.Errorf("a miss on its recording's path: summary %q, hint does not name the path whole (%s):\n%s", r.ActualSummary, want, r.NextSteps)
	}
	t.Cleanup(logcontent.Set(true))
	r = lifetimeMiss(t)
	if want := `pathPrefix: "/api/users/kp-live-id"}]), or record again`; !strings.Contains(r.NextSteps, want) || r.ContentWithheld {
		t.Errorf("with the switch on the hint does not name the call's path (%s):\n%s", want, r.NextSteps)
	}
}

// summaryPath reads the path of a summary: without its query, and "/" for a
// summary that names none.
func TestSummaryPath(t *testing.T) {
	for in, want := range map[string]string{
		"GET /api/users/{}":      "/api/users/{}",
		"POST /{}/{}":            "/{}/{}",
		"GET /a?b=c":             "/a",
		"GET /":                  "/",
		"GET":                    "/",
		"":                       "/",
		"GET api":                "/",
		"connection phase: x /y": "/",
	} {
		if got := summaryPath(in); got != want {
			t.Errorf("summaryPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// routeVariableMiss is, under the proven switch, the miss of a call whose
// closest recording declares a test variable on its route (orderId at
// url.path[1]) and differs from it in another segment: the call is
// /orders/recval/items/kp-live-item, the recording
// /orders/recval/items/kp-rec-item.
func routeVariableMiss(t *testing.T) *models.MockMismatchReport {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	read := at(varMock("mock-2", "GET", "http://store/orders/recval/items/kp-rec-item", "", 200, `{}`,
		models.MockVariables{"orderId": useV("recval", []string{"url.path[1]"})}), 2)
	db := &varDb{perTest: []*models.Mock{read}}
	ok, _, diag := matchVar(t, h, db, liveReq("GET", "/orders/recval/items/kp-live-item", ""))
	if ok || diag == nil {
		t.Fatalf("want a miss, got ok=%v diag=%+v", ok, diag)
	}
	return h.buildHTTPMismatchReport(makeReq("GET", "/orders/recval/items/kp-live-item"), nil, db, nil, nil, nil, diag)
}

// A miss on a route that declares a test variable names its path escaped,
// with the variable's places written {} (withRoutePlaces), and by default the
// segment its path diff finds differs is written {} too: the summary the
// report keeps is the one its builder withheld, with the diff's values to
// read, so adding the variable's places loses none of the diff's holes. Not
// in the report, not through the agent's door, not in the miss's shape. With
// the switch on the summary is the escaped path whole, as it was.
func TestARouteVariableMissKeepsItsPathDiffHoles(t *testing.T) {
	r := routeVariableMiss(t)
	door := models.UnmatchedCall{Protocol: r.Protocol, ActualSummary: r.ActualSummary, Diff: r.Diff, NextSteps: r.NextSteps,
		FieldDiffs: r.FieldDiffs, VariablePlaces: r.VariablePlaces, ContentWithheld: r.ContentWithheld}.ReportContent()
	const want = "GET /orders/{}/items/{}"
	for name, got := range map[string]string{"summary": r.ActualSummary, "door's summary": door.ActualSummary, "shape": models.MissShape(door)} {
		if got != want {
			t.Errorf("the default %s is %q, want %q", name, got, want)
		}
	}
	for _, v := range []string{"kp-live-item", "kp-rec-item"} {
		if text := reportText(t, r); strings.Contains(text, v) {
			t.Errorf("the default report carries %q:\n%s", v, text)
		}
	}
	if !slices.Contains(r.VariablePlaces, "url.path[1]") || !r.ContentWithheld {
		t.Errorf("the report does not name the variable's place: %+v", r.VariablePlaces)
	}
	t.Cleanup(logcontent.Set(true))
	if r := routeVariableMiss(t); r.ActualSummary != "GET /orders/recval/items/kp-live-item" || r.ContentWithheld ||
		!slices.Contains(r.VariablePlaces, "url.path[1]") {
		t.Errorf("with the switch on the summary is %q, places %v", r.ActualSummary, r.VariablePlaces)
	}
}

// sameCallMiss is the miss of a call that is its closest recording to the
// byte, as a call misses when the mock that answered it is not this test's
// to serve: no field differs, and the two rendered requests are the same.
func sameCallMiss(t *testing.T) *models.MockMismatchReport {
	t.Helper()
	mock := httpMockWithReq("mock-1", "POST", "http://users.internal:8080/api/users?customer=kp-rec-cust",
		`{"email":"kp-rec@example.com"}`, map[string]string{"Content-Type": "application/json"}, nil)
	req := makeReqWithQuery("POST", "/api/users", "customer=kp-rec-cust")
	req.Header.Set("Content-Type", "application/json")
	diag := &matchDiag{phase: models.MatchPhaseBody, candidates: 1, schemaMatched: []*models.Mock{mock}}
	return newHTTP().buildHTTPMismatchReport(req, []byte(`{"email":"kp-rec@example.com"}`), &mockMemDb{mocks: []*models.Mock{mock}}, nil, nil, nil, diag)
}

// A default report drops the rendered requests and says whether they were
// the same, which a reader told a mock keploy did not serve from a changed
// call by: true for a call that is its closest recording, false for one that
// is not. With the switch on the requests are kept, the same, and nothing
// else is said.
func TestADefaultHTTPReportSaysItsRequestsWereTheSame(t *testing.T) {
	r := sameCallMiss(t)
	if len(r.FieldDiffs) != 0 || !r.RequestsIdentical || r.ClosestMockReq != "" || r.ReceivedReq != "" || !r.ContentWithheld {
		t.Errorf("the default report of a call that is its closest recording: diffs %+v, identical %v, requests %q %q", r.FieldDiffs, r.RequestsIdentical, r.ClosestMockReq, r.ReceivedReq)
	}
	if text := reportText(t, r); strings.Contains(text, "kp-rec") {
		t.Errorf("the default report carries the request:\n%s", text)
	}
	if r := piiMiss(t); r.RequestsIdentical {
		t.Errorf("the default report of a call with other values says its requests were the same")
	}
	t.Cleanup(logcontent.Set(true))
	r = sameCallMiss(t)
	if r.RequestsIdentical || r.ClosestMockReq == "" || r.ClosestMockReq != r.ReceivedReq || r.ContentWithheld {
		t.Errorf("with the switch on: identical %v, requests %q %q", r.RequestsIdentical, r.ClosestMockReq, r.ReceivedReq)
	}
}
