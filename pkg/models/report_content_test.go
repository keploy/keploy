package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils/logcontent"
	"gopkg.in/yaml.v3"
)

// reportCanaries are values an application sent or keploy recorded, planted
// in every field of a report that can carry one.
var reportCanaries = []string{
	"kp-canary-rec-email", "kp-canary-live-email", "kp-canary-rec-ssn", "kp-canary-live-ssn",
	"kp-canary-rec-type", "kp-canary-live-type", "kp-canary-gone", "kp-canary-new",
	"kp-canary-sql-literal", "kp-canary-sql-param", "kp-canary-query-value",
	"kp-canary-diff-value", "kp-canary-mock-req", "kp-canary-live-req",
}

func contentCall() UnmatchedCall {
	return UnmatchedCall{
		Protocol:      "HTTP",
		ActualSummary: "POST /api/users?token=kp-canary-query-value",
		Destination:   "users.internal:8080",
		ClosestMock:   "mock-1",
		MatchPhase:    MatchPhaseBody,
		Diff:          `body.email: recorded "kp-canary-diff-value"`,
		NextSteps:     "Only values drifted (body.email).",
		FieldDiffs: []MockFieldDiff{
			{Path: "body.email", Kind: DiffKindValueChanged, Expected: "kp-canary-rec-email", Actual: "kp-canary-live-email"},
			{Path: "body.ssn", Kind: DiffKindVariable, Expected: "kp-canary-rec-ssn", Actual: "kp-canary-live-ssn"},
			{Path: "body.n", Kind: DiffKindTypeChanged, Expected: "string: kp-canary-rec-type", Actual: "number: kp-canary-live-type"},
			{Path: "header.X-Gone", Kind: DiffKindMissingInLive, Expected: "kp-canary-gone"},
			{Path: "query.extra", Kind: DiffKindMissingInMock, Actual: "kp-canary-new"},
		},
		ClosestMockReq: "POST /api/users\n\n{\"email\":\"kp-canary-mock-req\"}",
		ReceivedReq:    "POST /api/users\n\n{\"email\":\"kp-canary-live-req\"}",
		CandidateCount: 3,
	}
}

// encodedCall is c as the test-run report and the agent's API carry it.
func encodedCall(t *testing.T, c UnmatchedCall) string {
	t.Helper()
	y, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(y) + string(j)
}

// A call without content carries none of the values its fields held, in the
// report file or on the wire, and still says what differed: every field's
// path and kind, the counts, the call, its upstream and closest mock.
func TestACallWithoutContentSaysWhatDifferedAndNoValue(t *testing.T) {
	in := contentCall()
	out := in.WithoutContent()
	enc := encodedCall(t, out)
	for _, c := range reportCanaries {
		if strings.Contains(enc, c) {
			t.Errorf("a call without content carries %q:\n%s", c, enc)
		}
	}
	if !out.ContentWithheld || !strings.Contains(enc, "content_withheld: true") {
		t.Errorf("a call without content does not say so:\n%s", enc)
	}
	if out.ActualSummary != "POST /api/users" {
		t.Errorf("summary = %q, want the method and path without the query", out.ActualSummary)
	}
	if len(out.FieldDiffs) != len(in.FieldDiffs) {
		t.Fatalf("%d field diffs, want %d", len(out.FieldDiffs), len(in.FieldDiffs))
	}
	for i, d := range out.FieldDiffs {
		if d.Path != in.FieldDiffs[i].Path || d.Kind != in.FieldDiffs[i].Kind || d.Expected != "" || d.Actual != "" {
			t.Errorf("field diff %d = %+v, want %s %s without values", i, d, in.FieldDiffs[i].Path, in.FieldDiffs[i].Kind)
		}
	}
	// A call the builder did not withhold: its diff is text keploy did not
	// vet, so it is written again from the paths and kinds.
	want := "body.email: value changed; body.ssn: not the test variable's value; body.n: type changed; header.X-Gone: absent in live call; query.extra: absent in recording"
	if out.Diff != want {
		t.Errorf("diff = %q, want %q", out.Diff, want)
	}
	if out.Destination != in.Destination || out.ClosestMock != in.ClosestMock || out.CandidateCount != in.CandidateCount ||
		out.MatchPhase != in.MatchPhase || out.NextSteps != in.NextSteps || out.Protocol != in.Protocol {
		t.Errorf("a call without content lost what is not content: %+v", out)
	}
	// The caller's call is not changed: its field diffs may be shared.
	if !reflect.DeepEqual(in, contentCall()) {
		t.Errorf("WithoutContent changed the call it was given: %+v", in)
	}
}

// A report the builder withheld keeps its own words, the diff and the
// summary, and loses again any content written into it since.
func TestAWithheldReportKeepsItsWordsAndLosesLaterContent(t *testing.T) {
	r := MockMismatchReport{
		Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT ? FROM t", ContentWithheld: true,
		Diff:           "actual: SELECT ? FROM t",
		FieldDiffs:     []MockFieldDiff{{Path: "body.a", Kind: DiffKindValueChanged, Expected: "kp-canary-rec-email"}},
		ClosestMockReq: "kp-canary-mock-req",
	}
	out := r.WithoutContent()
	if out.Diff != r.Diff || out.ActualSummary != r.ActualSummary {
		t.Errorf("a withheld report's words changed: diff %q, summary %q", out.Diff, out.ActualSummary)
	}
	if out.ClosestMockReq != "" || out.FieldDiffs[0].Expected != "" || !out.ContentWithheld {
		t.Errorf("content written into a withheld report survives: %+v", out)
	}
	// Withholding twice is withholding once.
	if again := out.WithoutContent(); !reflect.DeepEqual(again, out) {
		t.Errorf("withheld again: %+v, want %+v", again, out)
	}
}

// A writer or printer keeps a call whole with the switch on, as it did, and
// withholds it with the switch off.
func TestReportContentFollowsTheSwitch(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	if got := contentCall().ReportContent(); !reflect.DeepEqual(got, contentCall()) {
		t.Errorf("with the switch on the call changed: %+v", got)
	}
	logcontent.Set(false)
	if got := contentCall().ReportContent(); !reflect.DeepEqual(got, contentCall().WithoutContent()) {
		t.Errorf("with the switch off the call is %+v, want it without content", got)
	}
}

func TestSummaryWithoutContent(t *testing.T) {
	for _, tc := range []struct {
		protocol, in string
		places       []string
		want         string
	}{
		{"MySQL", "COM_QUERY SELECT * FROM users WHERE email = 'kp-canary-sql-literal' AND id = 42", nil, "COM_QUERY SELECT * FROM users WHERE email = ? AND id = ?"},
		{"MySQL", "COM_STMT_EXECUTE SELECT * FROM t WHERE a = ? params=[kp-canary-sql-param]", nil, "COM_STMT_EXECUTE SELECT * FROM t WHERE a = ?"},
		{"PostgreSQL", "SELECT 'kp-canary-sql-literal'", nil, "SELECT ?"},
		{"HTTP", "GET /users/42?token=kp-canary-query-value", nil, "GET /users/42"},
		{"HTTP/2", "POST /v1/items?q=kp-canary-query-value", nil, "POST /v1/items"},
		// A segment keploy knows holds a value is written {}: a test
		// variable's place, or one a field diff names. No other is guessed at.
		{"HTTP", "GET /users/kp-canary-path/orders/42", []string{"url.path[1]", "body.id", "url.path[9]"}, "GET /users/{}/orders/42"},
		{"HTTP", "GET /users/kp-canary-path?x=kp-canary-query-value", []string{"url.path[1]"}, "GET /users/{}"},
		{"gRPC", "/pkg.v1.Users/Get", []string{"url.path[1]"}, "/pkg.v1.Users/{}"},
		{"HTTP", "GET /users/42", nil, "GET /users/42"},
		{"MongoDB", "find on users", []string{"url.path[0]"}, "find on users"},
		{"Redis", "GET", nil, "GET"},
		{"Redis", "GET /a/b?c", []string{"url.path[0]"}, "GET /a/b?c"},
		{ProtocolUnknown, "mongo find: kp-canary-diff-value", nil, WithheldValue},
		{ProtocolUnknown, "", nil, ""},
	} {
		if got := SummaryWithoutContent(tc.protocol, tc.in, tc.places); got != tc.want {
			t.Errorf("SummaryWithoutContent(%q, %q, %q) = %q, want %q", tc.protocol, tc.in, tc.places, got, tc.want)
		}
	}
}

// A call withheld again keeps its SQL and diff, the builder's words, and
// writes {} at every place it has come to know since: the summary of a call
// whose test variable's place was found after it was first withheld.
func TestAWithheldCallHolesThePlacesItLearnedSince(t *testing.T) {
	c := UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /orders/kp-canary-path", ContentWithheld: true,
		Diff: "mock mock-2: variable orderId is (withheld)", VariablePlaces: []string{"url.path[1]"}}
	out := c.WithoutContent()
	if out.ActualSummary != "GET /orders/{}" || out.Diff != c.Diff {
		t.Errorf("withheld again: summary %q, diff %q", out.ActualSummary, out.Diff)
	}
	sql := UnmatchedCall{Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT \"Users\" FROM t", ContentWithheld: true}
	if out := sql.WithoutContent(); out.ActualSummary != sql.ActualSummary {
		t.Errorf("a withheld SQL summary was shaped again: %q", out.ActualSummary)
	}
}

// A miss whose path is not the closest recording's says so in a field diff
// of the whole path, whose live value is the summary's path. Without
// content the diff loses its values, and the summary loses the segments the
// diff finds differ, at the places the two paths line up and past them: the
// segments the recording has there too are kept, so the report still says
// which call it was. A report already withheld, at the agent's door or in a
// report file, keeps the summary its builder made: its diff has no values
// left to compare, and comparing against none would hole every segment.
func TestAPathDiffHolesTheSegmentsItFindsDiffer(t *testing.T) {
	pathDiff := func(recorded, live string) MockFieldDiff {
		return MockFieldDiff{Path: PathFieldDiff, Kind: DiffKindValueChanged, Expected: recorded, Actual: live}
	}
	for _, c := range []struct {
		name, summary, recorded, want string
	}{
		{"one segment", "GET /api/users/kp-live-id", "/api/users/kp-rec-id", "GET /api/users/{}"},
		{"the query cut too", "GET /api/users/kp-live-id?x=kp-q", "/api/users/kp-rec-id", "GET /api/users/{}"},
		{"two segments", "GET /api/kp-live-org/users/kp-live-id", "/api/kp-rec-org/users/kp-rec-id", "GET /api/{}/users/{}"},
		{"past the recorded end", "GET /api/users/kp-live-id/kp-live-sub", "/api/users", "GET /api/users/{}/{}"},
		{"shorter than the recording", "GET /api/kp-live-id", "/api/users/kp-rec-id", "GET /api/{}"},
		{"shifted: what cannot be lined up", "GET /v2/api/users/kp-live-id", "/api/users/kp-rec-id", "GET /{}/{}/{}/{}"},
		{"no recorded path", "GET /api/kp-live-id", "", "GET /{}/{}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			call := UnmatchedCall{Protocol: "HTTP", ActualSummary: c.summary,
				FieldDiffs: []MockFieldDiff{pathDiff(c.recorded, strings.SplitN(strings.TrimPrefix(c.summary, "GET "), "?", 2)[0])}}
			out := call.WithoutContent()
			if out.ActualSummary != c.want {
				t.Errorf("summary %q, want %q", out.ActualSummary, c.want)
			}
			if out.FieldDiffs[0] != (MockFieldDiff{Path: PathFieldDiff, Kind: DiffKindValueChanged}) || out.Diff != "path: value changed" {
				t.Errorf("the path diff is %+v, the diff %q", out.FieldDiffs, out.Diff)
			}
			r := MockMismatchReport{Protocol: "HTTP", ActualSummary: c.summary, FieldDiffs: call.FieldDiffs}
			if got := r.WithoutContent().ActualSummary; got != c.want {
				t.Errorf("the report's summary %q, want %q", got, c.want)
			}
			// At a later door, withheld: kept as made.
			if again := out.WithoutContent(); again.ActualSummary != c.want {
				t.Errorf("withheld again, the summary is %q, want %q", again.ActualSummary, c.want)
			}
		})
	}
	// A diff of any other field is no path's: the summary keeps its path.
	other := UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/users/42",
		FieldDiffs: []MockFieldDiff{{Path: "body.path", Kind: DiffKindValueChanged, Expected: "/x", Actual: "/y"}}}
	if got := other.WithoutContent().ActualSummary; got != "GET /api/users/42" {
		t.Errorf("a body field named path holed the summary: %q", got)
	}
	// With the switch on a writer keeps the call whole.
	t.Cleanup(logcontent.Set(true))
	on := UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/users/kp-live-id",
		FieldDiffs: []MockFieldDiff{pathDiff("/api/users/kp-rec-id", "/api/users/kp-live-id")}}
	if got := on.ReportContent(); got.ActualSummary != on.ActualSummary || got.FieldDiffs[0].Actual != "/api/users/kp-live-id" {
		t.Errorf("with the switch on the call is %+v", got)
	}
}

// A report file read with the switch off loses the values of every call it
// holds, its tests' and its set's, and the report it was read from is left
// as it was. With the switch on it is read as written.
func TestAReportFileIsReadWithoutContentByDefault(t *testing.T) {
	file := func() *TestReport {
		return &TestReport{Name: "test-set-0", Status: "FAILED",
			Tests:          []TestResult{{TestCaseID: "test-1", FailureInfo: FailureInfo{UnmatchedCalls: []UnmatchedCall{contentCall()}}}, {TestCaseID: "test-2"}},
			UnclaimedCalls: []UnmatchedCall{contentCall()},
		}
	}
	in := file()
	out := in.ReportContent()
	enc := encodedCall(t, out.Tests[0].FailureInfo.UnmatchedCalls[0]) + encodedCall(t, out.UnclaimedCalls[0])
	for _, c := range reportCanaries {
		if strings.Contains(enc, c) {
			t.Errorf("a report file read by default carries %q", c)
		}
	}
	if out.Name != in.Name || out.Status != in.Status || len(out.Tests) != 2 || out.Tests[1].TestCaseID != "test-2" ||
		out.Tests[1].FailureInfo.UnmatchedCalls != nil || !out.Tests[0].FailureInfo.UnmatchedCalls[0].ContentWithheld {
		t.Errorf("a report file read by default lost what is not content: %+v", out)
	}
	if !reflect.DeepEqual(in, file()) {
		t.Error("reading a report file without its content changed the report it was read from")
	}
	if (*TestReport)(nil).ReportContent() != nil || ReportContent(nil) != nil {
		t.Error("nothing read is not nothing")
	}
	t.Cleanup(logcontent.Set(true))
	if got := in.ReportContent(); got != in {
		t.Errorf("with the switch on the report file is not read as written: %+v", got)
	}
}

// A value a parser names in its own words is quoted as it was with the switch
// on, and withheld with it off.
func TestReportValuesFollowTheSwitch(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	if got := ReportQuote(`a"b`); got != `"a\"b"` {
		t.Errorf("ReportQuote on = %s", got)
	}
	if got := ReportValue(`{"x":1}`); got != `{"x":1}` {
		t.Errorf("ReportValue on = %s", got)
	}
	if got := ReportSQL("SELECT 'x'"); got != "SELECT 'x'" {
		t.Errorf("ReportSQL on = %s", got)
	}
	logcontent.Set(false)
	if got := ReportQuote("kp-canary-diff-value"); got != WithheldValue {
		t.Errorf("ReportQuote off = %s", got)
	}
	if got := ReportValue("kp-canary-diff-value"); got != WithheldValue {
		t.Errorf("ReportValue off = %s", got)
	}
	if got := ReportSQL("SELECT 'kp-canary-sql-literal'"); got != "SELECT ?" {
		t.Errorf("ReportSQL off = %s", got)
	}
}

// Rendered with values, the diffs read as they always did; withheld, they
// name each field and how it differs.
func TestRenderFieldDiffs(t *testing.T) {
	diffs := contentCall().FieldDiffs
	want := `body.email: recorded "kp-canary-rec-email", live "kp-canary-live-email"; ` +
		`body.ssn: recorded "kp-canary-rec-ssn", live "kp-canary-live-ssn"; ` +
		`body.n: type changed (recorded string: kp-canary-rec-type, live number: kp-canary-live-type); ` +
		`header.X-Gone: recorded "kp-canary-gone", absent in live call; ` +
		`query.extra: live "kp-canary-new", absent in recording`
	if got := RenderFieldDiffs(diffs, false); got != want {
		t.Errorf("with values:\n got %s\nwant %s", got, want)
	}
	if got := RenderFieldDiffs(nil, true); got != "" {
		t.Errorf("no diffs rendered %q", got)
	}
}
