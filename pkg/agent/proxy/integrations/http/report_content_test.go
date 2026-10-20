package http

import (
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
