package models

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/models/postgres"
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
		{"the root path", "GET /", "/kp-rec-id", "GET /"},
		{"a trailing slash", "GET /api/users/", "/api/users/kp-rec-id", "GET /api/users/"},
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

// A report without content drops its two rendered requests, and a reader
// that compared them (cloud triage: a call the same as its closest mock is
// a mock keploy did not serve, not a change of the application's) reads the
// comparison from RequestsIdentical instead: true when the two were byte for
// byte the same as they were dropped, false when they differed or either was
// not rendered. It is made where the requests are dropped, while the report
// still has them; a report withheld before keeps it, unless a request was
// written into it since (a not-owned miss's recording and its call, both
// rendered again), when it says what the report holds now: only one written
// is not the same. With the switch on nothing is dropped and nothing is set:
// the reader compares the two, as before.
func TestAWithheldReportSaysWhetherItsRequestsWereTheSame(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mock, live    string
		withheldAfter bool
		was           bool
		want          bool
	}{
		{name: "the same requests", mock: "GET /a kp-canary-mock-req", live: "GET /a kp-canary-mock-req", want: true},
		{name: "other requests", mock: "GET /a kp-canary-mock-req", live: "GET /a kp-canary-live-req"},
		{name: "none rendered"},
		{name: "only the recording rendered", mock: "GET /a kp-canary-mock-req"},
		{name: "withheld before, the same", withheldAfter: true, was: true, want: true},
		{name: "withheld before, other", withheldAfter: true},
		{name: "withheld before, only a recording written since", mock: "GET /b kp-canary-mock-req", withheldAfter: true, was: true},
		{name: "withheld before, both written since, the same", mock: "GET /b kp-canary-mock-req", live: "GET /b kp-canary-mock-req", withheldAfter: true, want: true},
		{name: "withheld before, both written since, other", mock: "GET /b kp-canary-mock-req", live: "GET /b kp-canary-live-req", withheldAfter: true, was: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /a", ClosestMock: "mock-1",
				ClosestMockReq: tc.mock, ReceivedReq: tc.live, ContentWithheld: tc.withheldAfter, RequestsIdentical: tc.was}
			got := r.WithoutContent()
			if got.RequestsIdentical != tc.want || got.ClosestMockReq != "" || got.ReceivedReq != "" || !got.ContentWithheld {
				t.Errorf("report: identical %v (want %v), requests %q %q, withheld %v", got.RequestsIdentical, tc.want, got.ClosestMockReq, got.ReceivedReq, got.ContentWithheld)
			}
			c := UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /a", ClosestMock: "mock-1",
				ClosestMockReq: tc.mock, ReceivedReq: tc.live, ContentWithheld: tc.withheldAfter, RequestsIdentical: tc.was}.ReportContent()
			if c.RequestsIdentical != tc.want || c.ClosestMockReq != "" || c.ReceivedReq != "" {
				t.Errorf("call: identical %v (want %v), requests %q %q", c.RequestsIdentical, tc.want, c.ClosestMockReq, c.ReceivedReq)
			}
		})
	}
	t.Cleanup(logcontent.Set(true))
	c := UnmatchedCall{Protocol: "HTTP", ClosestMock: "mock-1", ClosestMockReq: "GET /a", ReceivedReq: "GET /a"}.ReportContent()
	if c.RequestsIdentical || c.ClosestMockReq != "GET /a" || c.ReceivedReq != "GET /a" {
		t.Errorf("with the switch on the call is %+v", c)
	}
	b, err := yaml.Marshal(c)
	if err != nil || strings.Contains(string(b), "requests_identical") {
		t.Errorf("with the switch on the call's yaml names requests_identical: %s %v", b, err)
	}
}

// The places of a call's path that are not its recording's are those whose
// segment is another at the same place, or past the recording's end; a
// summary that names no path has none.
func TestPathPlacesOtherThanAreTheSegmentsNotRecorded(t *testing.T) {
	for _, tc := range []struct {
		summary, recorded string
		want              []string
	}{
		{"GET /api/users/kp-live-id", "/api/users/kp-rec-id", []string{"url.path[2]"}},
		{"GET /api/users/kp-live-id?v=2", "/api/users/kp-rec-id", []string{"url.path[2]"}},
		{"GET /a/b/c", "/a", []string{"url.path[1]", "url.path[2]"}},
		{"GET /x/b", "/a/b", []string{"url.path[0]"}},
		{"GET /a/b", "/a/b", nil},
		{"GET /a", "/a/b/c", nil},
		{"find on users", "/a", nil},
	} {
		if got := PathPlacesOtherThan(tc.summary, tc.recorded); !slices.Equal(got, tc.want) {
			t.Errorf("PathPlacesOtherThan(%q, %q) = %v, want %v", tc.summary, tc.recorded, got, tc.want)
		}
	}
}

// A report's next steps are its parser's words, and the one piece of the
// call they quote is its summary, or the summary's path (the session-call
// hint's pathPrefix, quoted as Go quotes a string). A report read without
// content quotes them there as its summary now names them, whoever withheld
// it and whenever the summary gained its holes: from a report made with the
// switch on (an agent opted in under a CLI that is not, a report file written
// with it on), and in a report withheld before whose summary gained places
// since. Words that quote nothing of the call are kept as they were.
func TestTheWordsAReportKeepsQuoteItsSummaryAsItIsWithheld(t *testing.T) {
	hint := func(path string) string {
		return "declare it (sessionCalls: [{host: \"users.internal:8080\", method: \"GET\", pathPrefix: " + strconv.Quote(path) + "}]), or record again."
	}
	for _, tc := range []struct {
		name               string
		call               UnmatchedCall
		wantSummary        string
		wantNext, wantDiff string
		gone               []string
	}{
		{
			name: "a path diff's segment, in the hint and in the summary quoted whole",
			call: UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/users/kp-live-id?token=kp-q",
				NextSteps:  "The call GET /api/users/kp-live-id?token=kp-q missed. " + hint("/api/users/kp-live-id"),
				FieldDiffs: []MockFieldDiff{{Path: PathFieldDiff, Kind: DiffKindValueChanged, Expected: "/api/users/kp-rec-id", Actual: "/api/users/kp-live-id"}}},
			wantSummary: "GET /api/users/{}",
			wantNext:    "The call GET /api/users/{} missed. " + hint("/api/users/{}"),
			gone:        []string{"kp-live-id", "kp-rec-id", "kp-q"},
		},
		{
			name: "a segment Go quotes with escapes",
			call: UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/users/kp\"live\\id",
				NextSteps:  "The call " + strconv.Quote("GET /api/users/kp\"live\\id") + " missed. " + hint("/api/users/kp\"live\\id"),
				FieldDiffs: []MockFieldDiff{{Path: PathFieldDiff, Kind: DiffKindValueChanged, Expected: "/api/users/kp-rec-id", Actual: "/api/users/kp\"live\\id"}}},
			wantSummary: "GET /api/users/{}",
			wantNext:    "The call \"GET /api/users/{}\" missed. " + hint("/api/users/{}"),
			gone:        []string{"live", "kp-rec-id"},
		},
		{
			name: "a path of one segment: its quote, not every slash of the words",
			call: UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /kp-live-id",
				NextSteps:  "Serve it to every test and/or record again. " + hint("/kp-live-id"),
				FieldDiffs: []MockFieldDiff{{Path: PathFieldDiff, Kind: DiffKindValueChanged, Expected: "/kp-rec-id", Actual: "/kp-live-id"}}},
			wantSummary: "GET /{}",
			wantNext:    "Serve it to every test and/or record again. " + hint("/{}"),
			gone:        []string{"kp-live-id", "kp-rec-id"},
		},
		{
			name: "the root path: no segment, so nothing withheld",
			call: UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /",
				NextSteps:  "Serve it to every test and/or record again. " + hint("/"),
				FieldDiffs: []MockFieldDiff{{Path: PathFieldDiff, Kind: DiffKindValueChanged, Expected: "/kp-rec-id", Actual: "/"}}},
			wantSummary: "GET /",
			wantNext:    "Serve it to every test and/or record again. " + hint("/"),
			gone:        []string{"kp-rec-id"},
		},
		{
			name: "a variable's place, in a report withheld before",
			call: UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /orders/kp-live-order/items", ContentWithheld: true,
				VariablePlaces: []string{"url.path[1]"},
				Diff:           "the call GET /orders/kp-live-order/items: header.X: value changed",
				NextSteps:      hint("/orders/kp-live-order/items")},
			wantSummary: "GET /orders/{}/items",
			wantDiff:    "the call GET /orders/{}/items: header.X: value changed",
			wantNext:    hint("/orders/{}/items"),
			gone:        []string{"kp-live-order"},
		},
		{
			name: "a SQL statement's literals",
			call: UnmatchedCall{Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT * FROM t WHERE email = 'kp-canary-sql-literal'",
				NextSteps: "No recording answers \"COM_QUERY SELECT * FROM t WHERE email = 'kp-canary-sql-literal'\"."},
			wantSummary: "COM_QUERY SELECT * FROM t WHERE email = ?",
			wantNext:    "No recording answers \"COM_QUERY SELECT * FROM t WHERE email = ?\".",
			gone:        []string{"kp-canary-sql-literal"},
		},
		{
			name: "a miss named by its error",
			call: UnmatchedCall{Protocol: ProtocolUnknown, ActualSummary: "dial kp-canary-host: refused",
				NextSteps: "keploy failed: dial kp-canary-host: refused; check the agent's log."},
			wantSummary: WithheldValue,
			wantNext:    "keploy failed: " + WithheldValue + "; check the agent's log.",
			gone:        []string{"kp-canary-host"},
		},
		{
			name: "words that quote nothing of the call",
			call: UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/users/kp-live-id",
				NextSteps:  "Request structure changed since recording. Re-record the test set with 'keploy record'.",
				FieldDiffs: []MockFieldDiff{{Path: PathFieldDiff, Kind: DiffKindValueChanged, Expected: "/api/users/kp-rec-id", Actual: "/api/users/kp-live-id"}}},
			wantSummary: "GET /api/users/{}",
			wantNext:    "Request structure changed since recording. Re-record the test set with 'keploy record'.",
			gone:        []string{"kp-live-id"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := MockMismatchReport{Protocol: tc.call.Protocol, ActualSummary: tc.call.ActualSummary, Diff: tc.call.Diff,
				NextSteps: tc.call.NextSteps, FieldDiffs: tc.call.FieldDiffs, VariablePlaces: tc.call.VariablePlaces,
				ContentWithheld: tc.call.ContentWithheld}.WithoutContent()
			read := (&TestReport{Tests: []TestResult{{FailureInfo: FailureInfo{UnmatchedCalls: []UnmatchedCall{tc.call}}}}}).ReportContent()
			for door, got := range map[string]UnmatchedCall{
				"a call":        tc.call.WithoutContent(),
				"a report file": read.Tests[0].FailureInfo.UnmatchedCalls[0],
				"a report": {Protocol: report.Protocol, ActualSummary: report.ActualSummary, Diff: report.Diff, NextSteps: report.NextSteps,
					FieldDiffs: report.FieldDiffs, ContentWithheld: report.ContentWithheld},
			} {
				if got.ActualSummary != tc.wantSummary || got.NextSteps != tc.wantNext {
					t.Errorf("%s: summary %q, next steps %q; want %q, %q", door, got.ActualSummary, got.NextSteps, tc.wantSummary, tc.wantNext)
				}
				if tc.wantDiff != "" && got.Diff != tc.wantDiff {
					t.Errorf("%s: diff %q, want %q", door, got.Diff, tc.wantDiff)
				}
				enc := encodedCall(t, got)
				for _, v := range tc.gone {
					if strings.Contains(enc, v) {
						t.Errorf("%s carries %q:\n%s", door, v, enc)
					}
				}
				// Withholding twice is withholding once.
				if again := got.WithoutContent(); again.NextSteps != got.NextSteps || again.ActualSummary != got.ActualSummary || again.Diff != got.Diff {
					t.Errorf("%s withheld again: %+v", door, again)
				}
			}
		})
	}
	// With the switch on a writer keeps the words whole, as it did.
	t.Cleanup(logcontent.Set(true))
	on := UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/users/kp-live-id", NextSteps: hint("/api/users/kp-live-id"),
		FieldDiffs: []MockFieldDiff{{Path: PathFieldDiff, Kind: DiffKindValueChanged, Expected: "/api/users/kp-rec-id", Actual: "/api/users/kp-live-id"}}}
	if got := on.ReportContent(); got.NextSteps != on.NextSteps || got.ActualSummary != on.ActualSummary {
		t.Errorf("with the switch on the call is %+v", got)
	}
}

// mongoOp is a Mongo mock's operation as the recorder writes it today: the
// request's message as JSON (MongoOpMessage), its body section first.
func mongoOp(t *testing.T, sections ...string) string {
	t.Helper()
	b, err := json.Marshal(MongoOpMessage{Sections: sections})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A report names a recorded mock by what it recorded (a failed test's
// matched calls, a dependency row): by default without what the mock's kind
// holds of the application's content, and naming the call all the same. An
// HTTP mock keeps its method, host and path, without the query, fragment or
// credentials, and with every segment it declares for a test variable
// written {}; a Mongo mock keeps its command, which the recorder writes
// inside the whole request; a SQL one its operation without literals. With
// the switch on the name is the mock's summary, whole.
func TestAMockIsNamedWithoutItsContentByDefault(t *testing.T) {
	createUser := `{"createUser":"kp-canary-user","pwd":"kp-canary-pwd","roles":[],"$db":"admin"}`
	declared := &Mock{Kind: HTTP, Spec: MockSpec{
		HTTPReq:   &HTTPReq{Method: "GET", URL: "http://kp-user:kp-canary-secret@users.internal:8080/v1/users/kp-canary-id/orders?token=kp-canary-token#kp-canary-frag"},
		Variables: MockVariables{"userId": {Use: []string{"url.path[2]"}}},
	}}
	for _, c := range []struct {
		name string
		mock *Mock
		want string
	}{
		{"an HTTP mock", declared, "GET http://users.internal:8080/v1/users/{}/orders"},
		{"an HTTP mock with a query", &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "http://auth-svc:8080/v1/session?token=kp-canary-token&email=kp-canary@example.com"}}},
			"GET http://auth-svc:8080/v1/session"},
		{"an HTTP mock with a fragment", &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "http://svc:8080/docs#kp-canary-frag"}}}, "GET http://svc:8080/docs"},
		{"an HTTP mock with no path", &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "POST", URL: "http://svc:8080?kp-canary"}}}, "POST http://svc:8080"},
		{"a relative HTTP URL", &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "/v1/items?q=kp-canary"}}}, "GET /v1/items"},
		{"a Mongo request", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{}}, Metadata: map[string]string{"operation": mongoOp(t, "{ SectionSingle msg: "+createUser+" }")}}},
			"MongoDB createUser"},
		{"a Mongo request whose body follows a sequence", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{}}, Metadata: map[string]string{"operation": mongoOp(t,
			`{ SectionSingle identifier: documents , msgs: [ {"email":"kp-canary@example.com"} ] }`, `{ SectionSingle msg: {"insert":"users","$db":"kp-canary-db"} }`)}}},
			"MongoDB insert"},
		{"a Mongo command query", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{}}, Metadata: map[string]string{"operation": func() string {
			b, _ := json.Marshal(MongoOpQuery{FullCollectionName: "admin.$cmd", Query: `{"isMaster":1,"client":{"application":"kp-canary-app"}}`})
			return string(b)
		}()}}}, "MongoDB isMaster"},
		{"a Mongo query of a collection", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{}}, Metadata: map[string]string{"operation": func() string {
			b, _ := json.Marshal(MongoOpQuery{FullCollectionName: "shop.users", Query: `{"kp-canary-field":"kp-canary-value"}`})
			return string(b)
		}()}}}, "MongoDB"},
		{"an older recorder's Mongo text", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{}}, Metadata: map[string]string{
			"operation": `{ OpMsg flags: 0, sections: [{ SectionSingle msg: {"update":"url-shortener","$db":"keploy"} }, { SectionSingle identifier: updates , msgs: [ {"q":{"_id":"kp-canary-id"}} ] }], checksum: 0 }`}}},
			"MongoDB update"},
		{"an older recorder's Mongo text, its body second", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{}}, Metadata: map[string]string{
			"operation": `{ OpMsg flags: 0, sections: [{ SectionSingle identifier: updates , msgs: [ {"q":{"_id":"kp-canary-id"}} ] }, { SectionSingle msg: {"update":"t"} }], checksum: 0 }`}}},
			"MongoDB"},
		{"an older recorder's Mongo command query", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{}}, Metadata: map[string]string{
			"operation": `{ OpQuery flags: [SecondaryOK], fullCollectionName: admin.$cmd, numberToSkip: 0, numberToReturn: -1, query: {"isMaster": {"$numberInt":"1"},"client": {"application": "kp-canary-app"}}, returnFieldsSelector:  }`}}},
			"MongoDB isMaster"},
		{"an older recorder's Mongo collection query", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{}}, Metadata: map[string]string{
			"operation": `{ OpQuery flags: [], fullCollectionName: shop.users, numberToSkip: 0, numberToReturn: -1, query: {"kp-canary-field": 1}, returnFieldsSelector:  }`}}},
			"MongoDB"},
		{"Mongo text that reads as neither", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{}}, Metadata: map[string]string{"operation": `kp-canary {"find":"x"}`}}}, "MongoDB"},
		{"a Mongo mock with no operation", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{}}}}, "MongoDB"},
		{"a MySQL operation", &Mock{Kind: MySQL, Spec: MockSpec{MySQLRequests: []mysql.Request{{}}, Metadata: map[string]string{"operation": "SELECT * FROM t WHERE a = 'kp-canary'"}}}, "MySQL SELECT * FROM t WHERE a = ?"},
		{"a PostgreSQL operation", &Mock{Kind: PostgresV2, Spec: MockSpec{PostgresRequestsV2: []postgres.Request{{}}, Metadata: map[string]string{"operation": `SELECT "email" FROM "users" WHERE "id" = 'kp-canary'`}}},
			`PostgreSQL SELECT "email" FROM "users" WHERE "id" = ?`},
		{"a DNS name", &Mock{Kind: DNS, Spec: MockSpec{DNSReq: &DNSReq{Name: "users.internal."}}}, "DNS users.internal."},
		{"an HTTP/2 stream", &Mock{Kind: HTTP2, Spec: MockSpec{Metadata: map[string]string{"operation": "POST"}}}, "Http2 POST"},
		{"another kind's operation", &Mock{Kind: REDIS, Spec: MockSpec{Metadata: map[string]string{"operation": "GET session:kp-canary"}}}, "Redis"},
		{"another kind with none", &Mock{Kind: GENERIC}, "Generic"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := MockReportSummary(c.mock, true)
			if got != c.want || strings.Contains(got, "kp-canary") || strings.Contains(got, "kp-user") {
				t.Errorf("by default the mock is named %q, want %q", got, c.want)
			}
			// The report the name is written to is read by keploy report
			// (TestReport.ReportContent) as it was written: a matched call
			// and a dependency row, marked withheld as their writer marks
			// them.
			kind := string(c.mock.Kind)
			depType := DepTypeForKind(c.mock.Kind)
			file := func() *TestReport {
				return &TestReport{Tests: []TestResult{{
					FailureInfo: FailureInfo{MatchedCalls: []MatchedCall{{MockName: "mock-1", Protocol: kind, Summary: got, ContentWithheld: true}}},
					Result: Result{DepResult: []DepResult{{Name: DepRowName(0, depType, MockReportName(c.mock, MockSummaryFromSpec(c.mock), true)), Type: depType,
						Meta: []DepMetaResult{{Key: DepKeyPresence, Expected: DepPresenceConsumed, Actual: DepPresenceMissing}}, ContentWithheld: true}}},
				}}}
			}
			restore := logcontent.Set(false)
			read := file().ReportContent()
			restore()
			if !reflect.DeepEqual(read, file()) {
				t.Errorf("a report naming the mock %q is read by default as %+v, was written %+v", got, read.Tests[0], file().Tests[0])
			}
			if whole := MockReportSummary(c.mock, false); whole != MockSummaryFromSpec(c.mock) {
				t.Errorf("with the switch on the mock is named %q, want its summary %q", whole, MockSummaryFromSpec(c.mock))
			}
		})
	}
}

// A report file written with the switch on, or by an older keploy, is read
// with its tests' matched calls and dependency rows named as a report
// without content names them: no query, no Mongo request but its command, no
// SQL literal, by each call's protocol and each row's type; each still names
// the call. A row DepRowName did not write, and the overflow row, are kept.
// The file's own report is not changed, and with the switch on it is read as
// written.
func TestAReportFileNamesItsMatchedCallsAndDependenciesWithoutContent(t *testing.T) {
	t.Cleanup(logcontent.Set(false))
	mongo := "MongoDB " + mongoOp(t, `{ SectionSingle msg: {"createUser":"kp-canary-user","pwd":"kp-canary-pwd"} }`)
	file := func() *TestReport {
		return &TestReport{Tests: []TestResult{{
			FailureInfo: FailureInfo{MatchedCalls: []MatchedCall{
				{MockName: "mock-1", Protocol: "Http", Summary: "GET http://kp-user:kp-canary-pw@auth-svc:8080/v1/session?token=kp-canary-token"},
				{MockName: "mock-2", Protocol: "Mongo", Summary: mongo},
				{MockName: "mock-3", Protocol: "MySQL", Summary: "MySQL SELECT 'kp-canary-literal'"},
				{MockName: "mock-4", Protocol: "gRPC", Summary: "gRPC"},
			}},
			Result: Result{DepResult: []DepResult{
				{Name: DepRowName(0, "mongo", mongo), Type: "mongo"},
				{Name: DepRowName(1, "http", "GET auth-svc:8080/v1/session"), Type: "http"},
				{Name: DepRowName(2, "mysql", "mysql:3306 COM_QUERY"), Type: "mysql"},
				{Name: "deps[3] mongo hand-written kp-canary-row", Type: "mongo"},
				DepMissingOverflowRow(7),
			}},
		}}}
	}
	f := file()
	got := f.ReportContent().Tests[0]
	var summaries []string
	for _, c := range got.FailureInfo.MatchedCalls {
		summaries = append(summaries, c.Summary)
	}
	if want := []string{"GET http://auth-svc:8080/v1/session", "MongoDB createUser", "MySQL SELECT ?", "gRPC"}; !reflect.DeepEqual(summaries, want) {
		t.Errorf("matched calls read as %q, want %q", summaries, want)
	}
	var names []string
	for _, d := range got.Result.DepResult {
		names = append(names, d.Name)
	}
	if want := []string{"deps[0] mongo MongoDB createUser (presence)", "deps[1] http GET auth-svc:8080/v1/session (presence)", "deps[2] mysql mysql:3306 COM_QUERY (presence)",
		"deps[3] mongo hand-written kp-canary-row", DepMissingOverflowRow(7).Name}; !reflect.DeepEqual(names, want) {
		t.Errorf("dependency rows read as %q, want %q", names, want)
	}
	if !reflect.DeepEqual(f, file()) {
		t.Errorf("reading the file changed it: %+v", f.Tests[0])
	}
	logcontent.Set(true)
	if got := file().ReportContent(); !reflect.DeepEqual(got, file()) {
		t.Errorf("with the switch on the file is read as %+v", got.Tests[0])
	}
}
