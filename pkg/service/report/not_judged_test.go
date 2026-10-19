package report

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// notJudgedReport is a set whose test-1 passed and whose test-2's row says
// PASSED although keploy could not read what it needed to judge it (each
// FailureInfo flag the replayer sets under the proven switch).
func notJudgedReport(fi models.FailureInfo) map[string]*models.TestReport {
	return map[string]*models.TestReport{"test-set-0": {
		Total: 2, Success: 2, TimeTaken: "1s",
		Tests: []models.TestResult{
			{TestCaseID: "test-1", Status: models.TestStatusPassed},
			{TestCaseID: "test-2", Status: models.TestStatusPassed, FailureInfo: fi},
		},
	}}
}

var notJudgedCases = []struct {
	name string
	fi   models.FailureInfo
	why  string
}{
	{"unmatched calls unread", models.FailureInfo{UnmatchedCallsUnread: true}, "no recorded mock answered"},
	{"test mocks unread", models.FailureInfo{TestMocksUnread: true}, "which of its recorded calls it made"},
	{"variables unchecked", models.FailureInfo{VariablesUnchecked: "the learn step's table could not be read"}, "the learn step's table could not be read"},
	{"capture unread", models.FailureInfo{CaptureUnread: true}, "captured every call"},
	{"cannot capture (decision 20)", models.FailureInfo{CannotCapture: models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/run/app/db.sock"}}}, "a Unix socket keploy cannot capture (1 time(s)) while this test or one before it ran (/run/app/db.sock)"},
	{"an abandoned TLS handshake", models.FailureInfo{CannotCapture: models.CaptureLosses{{Kind: models.CaptureLossTLSAbandoned, Count: 1, What: "to 10.0.0.1:443"}}}, "libcurl clients such as PHP's Guzzle and pycurl: CURLOPT_CAINFO (Guzzle's verify, pycurl's CAINFO), which libcurl does not take from those variables) (to 10.0.0.1:443)"},
}

// A test keploy could not judge is not green in JUnit, whatever its row's
// status says: it is an <error>, counted in the suite's and the file's
// errors, so a CI system that gates on the JUnit file fails it as the run's
// exit code does (P002). It was a passing <testcase>.
func TestJUnitAnUnjudgedTestIsAnError(t *testing.T) {
	for _, tc := range notJudgedCases {
		t.Run(tc.name, func(t *testing.T) {
			suites := buildJUnitSuites(notJudgedReport(tc.fi))
			s := suites.Suites[0]
			if s.Errors != 1 || suites.Errors != 1 || s.Failures != 0 {
				t.Fatalf("suite errors %d, file errors %d, failures %d; want 1, 1, 0", s.Errors, suites.Errors, s.Failures)
			}
			c := s.Cases[1]
			if c.Error == nil || c.Error.Type != "NotJudged" || !strings.Contains(c.Error.Text, tc.why) || c.SystemOut != "" || c.Skipped != nil {
				t.Fatalf("test-2 = %+v; want an <error> of type NotJudged saying %q, and nothing else", c, tc.why)
			}
			if s.Cases[0].Error != nil {
				t.Fatal("test-1 was judged, and is not an error")
			}
			data, err := xml.Marshal(suites)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `<testsuites tests="2" failures="0" errors="1"`) || !strings.Contains(string(data), `<error message="`+notJudgedMessage+`" type="NotJudged">`) {
				t.Fatalf("the XML does not carry the error:\n%s", data)
			}
		})
	}
}

// A test that failed stays a <failure>, and one keploy skipped stays
// <skipped>: only a case that would otherwise read green is turned into an
// error. A report with no unjudged test has no errors attribute at all, so
// its XML is byte-identical.
func TestJUnitErrorsOnlyWhereTheCaseWouldReadGreen(t *testing.T) {
	reports := notJudgedReport(models.FailureInfo{TestMocksUnread: true})
	reports["test-set-0"].Tests[1].Status = models.TestStatusFailed
	reports["test-set-0"].Tests = append(reports["test-set-0"].Tests,
		models.TestResult{TestCaseID: "test-3", Status: models.TestStatusIgnored, FailureInfo: models.FailureInfo{TestMocksUnread: true}})
	s := buildJUnitSuites(reports).Suites[0]
	if s.Errors != 0 || s.Cases[1].Failure == nil || s.Cases[1].Error != nil || s.Cases[2].Skipped == nil || s.Cases[2].Error != nil {
		t.Fatalf("suite %+v; want test-2 a failure, test-3 skipped, no errors", s)
	}

	// The NDJSON marks the same cases: neither the failed nor the skipped one.
	var buf bytes.Buffer
	if err := writeNDJSON(&buf, buildTestVerdicts("test-run-0", reports)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "verdict_unknown") {
		t.Fatalf("a failed or a skipped test's line says its verdict is unknown:\n%s", buf.String())
	}

	data, err := xml.Marshal(buildJUnitSuites(notJudgedReport(models.FailureInfo{})))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "errors=") || strings.Contains(string(data), "<error") {
		t.Fatalf("a report with every test judged gained an error mark:\n%s", data)
	}
}

// The NDJSON says a test keploy could not judge has no verdict
// (verdict_unknown, with the reason), beside its row's status, so a consumer
// reading the status does not read PASSED as a verdict (P002). A judged test
// has neither field.
func TestNDJSONAnUnjudgedTestHasAnUnknownVerdict(t *testing.T) {
	for _, tc := range notJudgedCases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeNDJSON(&buf, buildTestVerdicts("test-run-0", notJudgedReport(tc.fi))); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("%d lines, want 2:\n%s", len(lines), buf.String())
			}
			var judged, unjudged map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &judged); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(lines[1]), &unjudged); err != nil {
				t.Fatal(err)
			}
			if _, ok := judged["verdict_unknown"]; ok {
				t.Fatalf("test-1 was judged, and says its verdict is unknown: %s", lines[0])
			}
			if _, ok := judged["verdict_unknown_reason"]; ok {
				t.Fatalf("test-1 was judged, and gives a reason it has no verdict: %s", lines[0])
			}
			if unjudged["verdict_unknown"] != true || !strings.Contains(unjudged["verdict_unknown_reason"].(string), tc.why) {
				t.Fatalf("test-2's line %s; want verdict_unknown true and a reason saying %q", lines[1], tc.why)
			}
		})
	}
}

// The proven model's evidence on a failed case (the calls with no recorded
// answer, the recorded calls not made) is rendered when the set was run
// under the switch, as its report says (TestReport.Proven), whatever the
// switch of the process rendering it: `keploy report --format junit` run
// without KEPLOY_PROVEN dropped it (P026). A report of a set run without the
// switch renders as before.
func TestJUnitRendersTheProvenEvidenceOfASetRunUnderTheSwitch(t *testing.T) {
	failed := models.TestResult{
		TestCaseID: "test-1", Kind: models.HTTP, Status: models.TestStatusFailed,
		Result: models.Result{StatusCode: models.IntResult{Normal: true}},
		FailureInfo: models.FailureInfo{
			UnmatchedCalls:  []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /charges"}},
			UnconsumedMocks: []models.UnconsumedMock{{Name: "mock-7", Kind: "Http"}},
		},
	}
	report := func(proven bool) map[string]*models.TestReport {
		return map[string]*models.TestReport{"test-set-0": {Total: 1, Failure: 1, Proven: proven, Tests: []models.TestResult{failed}}}
	}
	t.Setenv(provenswitch.Env, "0")
	body := buildJUnitSuites(report(true)).Suites[0].Cases[0].Failure.Text
	for _, want := range []string{"call with no recorded answer: HTTP POST /charges", "recorded call not made: mock mock-7 (Http)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("a set run under the switch, rendered without it: the failure does not say %q:\n%s", want, body)
		}
	}
	if body := buildJUnitSuites(report(false)).Suites[0].Cases[0].Failure.Text; strings.Contains(body, "no recorded answer") || strings.Contains(body, "mock-7") {
		t.Fatalf("a set run without the switch: the failure changed:\n%s", body)
	}
}

// A case with no verdict keeps the dependency detail a passing case shows.
func TestJUnitAnUnjudgedCaseKeepsItsDependencyDetail(t *testing.T) {
	reports := notJudgedReport(models.FailureInfo{TestMocksUnread: true})
	reports["test-set-0"].Tests[1].Result.DepResult = []models.DepResult{{Name: "deps[0] postgres SELECT", Type: "postgres",
		Meta: []models.DepMetaResult{{Key: models.DepKeyPresence, Expected: "consumed", Actual: "not consumed"}}}}
	c := buildJUnitSuites(reports).Suites[0].Cases[1]
	if c.Error == nil || !strings.Contains(c.SystemOut, "deps[0] postgres SELECT") {
		t.Fatalf("test-2 = %+v; want its <error> and its dependency detail in <system-out>", c)
	}
}

// A set run under the switch that keploy could not judge for a reason no
// test carries (its capture count, or its mock.real destinations, could not
// be read) is INTERNAL_ERR while every case may read green: a case of its
// own says it has no verdict. A set run without the switch is as before.
func TestJUnitASetKeployCouldNotJudgeIsAnError(t *testing.T) {
	report := func(proven bool) map[string]*models.TestReport {
		return map[string]*models.TestReport{"test-set-0": {
			Total: 1, Success: 1, Proven: proven, Status: string(models.TestSetStatusInternalErr),
			FailureReason: "not judged: keploy could not read whether its agent captured every call",
			Tests:         []models.TestResult{{TestCaseID: "test-1", Status: models.TestStatusPassed}},
		}}
	}
	s := buildJUnitSuites(report(true)).Suites[0]
	if s.Errors != 1 || s.Tests != 2 || len(s.Cases) != 2 {
		t.Fatalf("suite %+v; want one more case, an error", s)
	}
	if c := s.Cases[1]; c.Name != junitVerdictCase || c.Error == nil || !strings.Contains(c.Error.Text, "captured every call") {
		t.Fatalf("case %+v; want %q, an error with the set's reason", c, junitVerdictCase)
	}
	// A set run without the switch is not told unjudged: it did not pass
	// (TestJUnitASetThatDidNotPassIsNeverAGreenSuite).
	if s := buildJUnitSuites(report(false)).Suites[0]; s.Errors != 1 || len(s.Cases) != 2 || s.Cases[1].Error == nil || s.Cases[1].Error.Type != "TestSetNotPassed" {
		t.Fatalf("a set run without the switch: suite %+v; want its did-not-pass case, not a not-judged one", s)
	}
	// A case of it that failed does not make the set judged: the set's
	// reason is said all the same.
	failed := report(true)
	failed["test-set-0"].Tests = append(failed["test-set-0"].Tests, models.TestResult{TestCaseID: "test-2", Status: models.TestStatusFailed})
	if s := buildJUnitSuites(failed).Suites[0]; s.Errors != 1 || s.Failures != 1 || s.Cases[len(s.Cases)-1].Name != junitVerdictCase {
		t.Fatalf("suite %+v; want test-2's failure and the set's verdict case", s)
	}
}

// The NDJSON of a set run under the switch that keploy could not judge for a
// reason no test carries marks its other tests' verdicts unknown, with the
// set's reason, as its JUnit suite carries a "keploy verdict" error: the two
// formats agree. A set run without the switch is as before.
func TestNDJSONASetKeployCouldNotJudgeHasNoVerdicts(t *testing.T) {
	report := func(proven bool) map[string]*models.TestReport {
		return map[string]*models.TestReport{"test-set-0": {
			Total: 2, Success: 1, Failure: 1, Proven: proven, Status: string(models.TestSetStatusInternalErr),
			FailureReason: "not judged: keploy could not read the set's mock.real destinations",
			Tests: []models.TestResult{
				{TestCaseID: "test-1", Status: models.TestStatusPassed},
				{TestCaseID: "test-2", Status: models.TestStatusFailed},
			},
		}}
	}
	v := buildTestVerdicts("test-run-0", report(true))
	if !v[0].VerdictUnknown || !strings.Contains(v[0].VerdictUnknownReason, "mock.real destinations") {
		t.Fatalf("test-1 %+v; want its verdict unknown, with the set's reason", v[0])
	}
	if v[1].VerdictUnknown {
		t.Fatal("test-2 failed: its verdict is no less a verdict")
	}
	for _, v := range buildTestVerdicts("test-run-0", report(false)) {
		if v.VerdictUnknown {
			t.Fatalf("a set run without the switch: %+v; want it as before", v)
		}
	}
}

// A test whose exchange ended with nothing of its answer to compare (the
// stall rule, or the app's keepalives) is FAILED, so a reader of the status
// alone sees no pass, and marked not judged (FailureInfo.AnswerStalled): JUnit
// shows it as a NotJudged <error> saying what was waited for, not a
// <failure>, and the NDJSON marks its verdict unknown, with the same. A
// FAILED row without the mark stays a failure in both.
func TestAFailedRowWithNothingOfItsAnswerToCompareIsNotJudged(t *testing.T) {
	const why = "the app never answered; waited 1m0s of keploy's runnable time for GET /orders"
	reports := map[string]*models.TestReport{"test-set-0": {
		Total: 3, Success: 1, Failure: 2, TimeTaken: "1s", Proven: true,
		Tests: []models.TestResult{
			{TestCaseID: "test-1", Status: models.TestStatusPassed},
			{TestCaseID: "test-2", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{AnswerStalled: why}},
			{TestCaseID: "test-3", Status: models.TestStatusFailed},
		},
	}}
	s := buildJUnitSuites(reports).Suites[0]
	if c := s.Cases[1]; c.Error == nil || c.Error.Type != "NotJudged" || !strings.Contains(c.Error.Text, why) || c.Failure != nil {
		t.Fatalf("test-2 = %+v; want a NotJudged <error> saying %q, not a <failure>", c, why)
	}
	if c := s.Cases[2]; c.Failure == nil || c.Error != nil {
		t.Fatalf("test-3 = %+v; want its <failure>", c)
	}
	if s.Errors != 1 || s.Failures != 1 {
		t.Fatalf("suite errors %d, failures %d; want 1 and 1", s.Errors, s.Failures)
	}
	v := buildTestVerdicts("test-run-0", reports)
	if !v[1].VerdictUnknown || v[1].VerdictUnknownReason != why || v[1].Status != string(models.TestStatusFailed) {
		t.Fatalf("test-2 %+v; want FAILED with its verdict unknown, saying %q", v[1], why)
	}
	if v[2].VerdictUnknown {
		t.Fatalf("test-3 %+v; a failure on what was compared is a verdict", v[2])
	}
}
