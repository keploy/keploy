package report

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

type junitTestSuites struct {
	XMLName  xml.Name `xml:"testsuites"`
	Tests    int      `xml:"tests,attr"`
	Failures int      `xml:"failures,attr"`
	// Errors counts the cases in error (junitError). Omitted at 0, so a
	// report with none stays byte-identical.
	Errors int              `xml:"errors,attr,omitempty"`
	Time   string           `xml:"time,attr"`
	Suites []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	Name     string `xml:"name,attr"`
	Tests    int    `xml:"tests,attr"`
	Failures int    `xml:"failures,attr"`
	// Errors counts the suite's cases in error; omitted at 0.
	Errors  int    `xml:"errors,attr,omitempty"`
	Skipped int    `xml:"skipped,attr"`
	Time    string `xml:"time,attr"`
	// Properties carries suite-level facts that are not per-testcase. Emitted
	// only when there is one, so a report without any stays byte-identical
	// (the backward-compat contract this file documents). Today the only one
	// is capture verification: whether the agent tracked every process.
	Properties *junitProperties `xml:"properties,omitempty"`
	Cases      []junitTestCase  `xml:"testcase"`
}

type junitProperties struct {
	Property []junitProperty `xml:"property"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitTestCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	// Error marks a case in error (junitError).
	Error   *junitError   `xml:"error,omitempty"`
	Skipped *junitSkipped `xml:"skipped,omitempty"`
	// SystemOut carries information that must reach a CI consumer WITHOUT
	// changing the case's verdict — today, the dependencies a PASSED test
	// stopped exercising. <system-out> is the JUnit element every consumer
	// already renders as free text; a <failure> would flip the build red,
	// which is exactly what --assert-dependencies is for and must not happen
	// by default.
	SystemOut string `xml:"system-out,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr,omitempty"`
	Text    string `xml:",chardata"`
}

// junitError is the <error> of a case in error: one keploy could not judge
// (its row says PASSED or OBSOLETE, but what keploy needed to judge it could
// not be read or checked, models.FailureInfo.NotJudged, so nothing vouches for
// it), or a set's verdict case (junitVerdictCase). CI systems count an <error>
// as a failed case, as they should: the run that wrote the report exits
// non-zero, and its artifact must not read green.
type junitError struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// notJudgedMessage is the <error message> of a case with no verdict.
const notJudgedMessage = "not judged: keploy could not tell whether this test passed"

type junitSkipped struct {
	Message string `xml:"message,attr,omitempty"`
	// Text is the element body. Detail belongs HERE, not appended to the
	// message attribute: the attribute is what Jenkins/GitLab/Buildkite group
	// and display skipped cases by, so varying it per test would explode a
	// single "obsolete test case" bucket into one bucket per dependency
	// combination, and an unbounded attribute (one clause per unconsumed mock)
	// would put kilobytes on one XML line.
	//
	// The one exception is a case keploy quarantined: its message is
	// "quarantined by keploy (<reason>)", as a test framework's own skip
	// shows its reason (the proven model's contract), so it groups by reason.
	// It stays one line and bounded (quarantinedSkipMessage); the whole
	// reason is the body.
	Text string `xml:",chardata"`
}

// generateJUnit writes JUnit XML output for the collected test reports.
func (r *Report) generateJUnit(reports map[string]*models.TestReport, explained map[string]bool) error {
	suites := buildJUnitSuitesExplained(reports, explained)
	data, err := xml.MarshalIndent(suites, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JUnit XML: %w", err)
	}

	if _, err := r.out.WriteString(xml.Header); err != nil {
		return fmt.Errorf("failed to write XML header: %w", err)
	}
	if _, err := r.out.Write(data); err != nil {
		return fmt.Errorf("failed to write JUnit XML: %w", err)
	}
	if _, err := r.out.WriteString("\n"); err != nil {
		return fmt.Errorf("failed to write trailing newline: %w", err)
	}
	return r.out.Flush()
}

// buildJUnitSuites converts Keploy TestReports into JUnit XML structs.
//
// WHAT JUNIT DELIBERATELY DOES NOT CARRY: the dependency-assertion COVERAGE
// bit (models.Result.DepsChecked / the NDJSON `dependencies_checked`).
//
// A PASSED test whose dependency assertion ran and found nothing missing, and a
// PASSED test whose assertion never ran at all, both render as
// `<testcase ...></testcase>` with no <system-out>. They are byte-identical.
// That is not an oversight to be fixed by adding a property here, and a
// reviewer should not "fix" it without reading this:
//
//   - JUnit never makes a FALSE CLAIM either way. It says nothing about
//     dependencies for a green test, so a consumer cannot read a coverage
//     answer out of it and cannot be misled into treating "not checked" as
//     "checked and clean". The false-green this slice closes lives in the
//     surfaces that DO make a claim — the report YAML and the NDJSON — and it
//     is closed there.
//   - The obvious form (a suite-level
//     `<property name="keploy.dependencies_checked" value="0/3"/>` emitted
//     whenever some test in the suite is unchecked) CANNOT be added without
//     changing the XML of every pre-slice-4 report. A legacy report and a
//     modern all-unchecked run are indistinguishable on disk — both carry
//     DepsChecked=false on every test, because the legacy report has no such
//     field at all — so any rule that fires on "some test is unchecked" fires
//     on every legacy suite too, and the backward-compatibility contract for
//     this slice is that a report with no dependency data stays byte-identical.
//
// SO: dependency COVERAGE is not expressible in JUnit, and a CI system that
// consumes only the JUnit artifact cannot learn whether the run's dependency
// verdict was UNKNOWN — which, for an ordinary recording whose mocks carry no
// per-test tier tag, is the state EVERY test lands in. Consumers that need the
// bit must read `keploy report --format json` (`dependencies_checked` per
// verdict) or the persisted `deps_checked` in the test-set report YAML. The
// runtime WARN names the reason once per test set.
//
// What JUnit DOES carry is the dependency DETAIL when there is any: missing
// rows reach <failure> on a FAILED case, the <skipped> body on an OBSOLETE one
// and <system-out> on a green one (see depNonFailureBody).
func buildJUnitSuites(reports map[string]*models.TestReport) junitTestSuites {
	return buildJUnitSuitesExplained(reports, nil)
}

// buildJUnitSuitesExplained is buildJUnitSuites where explained names the
// sets a case of which failed or has no verdict, whether or not it is among
// the cases rendered (--test-case): those carry no did-not-pass case.
func buildJUnitSuitesExplained(reports map[string]*models.TestReport, explained map[string]bool) junitTestSuites {
	var totalTests, totalFailures, totalErrors int
	var totalDuration time.Duration
	suites := make([]junitTestSuite, 0, len(reports))

	// Sort test-set names for deterministic XML output.
	names := make([]string, 0, len(reports))
	for name := range reports {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		rep := reports[name]
		suite := buildJUnitSuiteOf(name, rep, explained[name])
		totalTests += suite.Tests
		totalFailures += suite.Failures
		totalErrors += suite.Errors
		if dur, err := parseTimeString(rep.TimeTaken); err == nil {
			totalDuration += dur
		} else {
			totalDuration += estimateDuration(rep.Tests)
		}
		suites = append(suites, suite)
	}

	return junitTestSuites{
		Tests:    totalTests,
		Failures: totalFailures,
		Errors:   totalErrors,
		Time:     fmtSeconds(totalDuration),
		Suites:   suites,
	}
}

func buildJUnitSuite(name string, rep *models.TestReport) junitTestSuite {
	return buildJUnitSuiteOf(name, rep, false)
}

// buildJUnitSuiteOf is a set's suite; explained says a case of it failed or
// has no verdict among all its cases, shown or not (--test-case).
func buildJUnitSuiteOf(name string, rep *models.TestReport, explained bool) junitTestSuite {
	cases := make([]junitTestCase, 0, len(rep.Tests))
	var failures, errs, skipped int
	// The proven model's evidence is rendered when the set was run under the
	// switch (rep.Proven), whatever the switch of the process rendering it;
	// for a report written before the marker, when the switch is on now.
	proven := rep.Proven || provenswitch.On()

	for _, t := range rep.Tests {
		tc := junitTestCase{
			Name:      t.TestCaseID,
			Classname: name,
			Time:      fmtTestTime(t.TimeTaken),
		}

		// A case keploy could not judge is an <error>, whatever its row's
		// status says, unless the row fails it on what was compared or
		// keploy skipped it (TestResult.VerdictUnknown: a FAILED row with
		// nothing of its answer to compare, AnswerStalled, is one keploy
		// could not judge): nothing vouches for it, so its artifact must
		// not read green. Its dependency detail is kept, as a passing
		// case's is.
		if why := t.VerdictUnknown(); len(why) > 0 {
			errs++
			tc.Error = &junitError{Message: notJudgedMessage, Type: "NotJudged", Text: strings.Join(why, "\n")}
			tc.SystemOut = depNonFailureBody(t.Result.DepResult)
			cases = append(cases, tc)
			continue
		}

		switch t.Status {
		case models.TestStatusFailed:
			failures++
			tc.Failure = failureOf(t, proven)
		case models.TestStatusObsolete:
			skipped++
			tc.Skipped = &junitSkipped{
				Message: obsoleteSkipMessage,
				Text:    depNonFailureBody(t.Result.DepResult),
			}
		case models.TestStatusIgnored:
			skipped++
			tc.Skipped = ignoredSkip(t)
		default:
			// PASSED (and anything else that is neither failed nor skipped).
			// A test whose response still matched while a recorded outgoing
			// call vanished lands HERE, not on the failed or obsolete arms —
			// it is the silent-green case the slice exists to expose. It gets
			// <system-out> so CI shows it without the status flipping;
			// --assert-dependencies is what turns it into a <failure>.
			tc.SystemOut = depNonFailureBody(t.Result.DepResult)
		}

		cases = append(cases, tc)
	}

	var suiteDur time.Duration
	if dur, err := parseTimeString(rep.TimeTaken); err == nil {
		suiteDur = dur
	} else {
		suiteDur = estimateDuration(rep.Tests)
	}

	suite := junitTestSuite{
		Name:     name,
		Tests:    rep.Total,
		Failures: failures,
		Errors:   errs,
		Skipped:  skipped,
		Time:     fmtSeconds(suiteDur),
		Cases:    cases,
	}
	// Capture verification is a suite-level fact, so it is a property, emitted
	// only when the run found something (a capture that was incomplete, or one
	// that could not be verified): a report without it stays byte-identical.
	//
	// An incomplete capture failed the test set while its tests may all have
	// passed (keploy cannot tell which test's calls the untracked process
	// made). A JUnit consumer gates on <failure> elements, not on properties,
	// so the suite gets one more test case, "keploy capture", failed with the
	// reason: a FAILED set is never a green JUnit file. "unverified" adds no
	// test case; the property says it. (Under the proven switch the set is
	// also not judged: the "keploy verdict" case below says so.)
	if cv := rep.CaptureVerification; cv != nil {
		suite.Properties = &junitProperties{Property: []junitProperty{
			{Name: "keploy.capture_verification", Value: string(cv.Status)},
			{Name: "keploy.capture_verification.reason", Value: cv.Reason},
		}}
		if cv.Status == models.CaptureIncompleteStatus {
			suite.Tests++
			suite.Failures++
			suite.Cases = append(suite.Cases, junitTestCase{
				Name:      junitCaptureCase,
				Classname: name,
				Time:      fmtSeconds(0),
				Failure: &junitFailure{
					Message: "capture incomplete",
					Type:    "CaptureIncomplete",
					Text:    cv.Reason,
				},
			})
		}
	}
	// A set run under the switch that is INTERNAL_ERR for a reason no test
	// carries (its capture count or its mock.real destinations could not be
	// read, a result could not be written, its run ended part-way) has no
	// verdict, whatever its cases say: a case of its own, an <error>, says
	// so with the set's reason, as its NDJSON lines do.
	if rep.Proven && rep.Status == string(models.TestSetStatusInternalErr) {
		suite.Tests++
		suite.Errors++
		suite.Cases = append(suite.Cases, junitTestCase{
			Name:      junitVerdictCase,
			Classname: name,
			Time:      fmtSeconds(0),
			Error:     &junitError{Message: notJudgedSetMessage, Type: "NotJudged", Text: rep.FailureReason},
		})
	}
	// mock.real's rule 4 failed the set while its tests may all have
	// passed: as an incomplete capture, a case of its own, failed.
	if why := rep.RealFailure; why != "" {
		suite.Tests++
		suite.Failures++
		suite.Cases = append(suite.Cases, junitTestCase{
			Name:      junitRealCase,
			Classname: name,
			Time:      fmtSeconds(0),
			Failure: &junitFailure{
				Message: "mock.real destination not reached",
				Type:    "RealNotReached",
				Text:    why,
			},
		})
	}
	// The calls the set's end read that no mock answered and that the agent
	// could not place outside every test's window (made in the last test's
	// window and filed after its read, or at a time it does not say) failed
	// the set while its tests may all have passed: a case of its own,
	// failed, naming each and where it was made, from its Kind. A call made
	// outside every test's window is background work, never here.
	if calls := rep.UnclaimedCalls; len(calls) > 0 {
		lines := make([]string, 0, len(calls))
		message := "a call in the set's last test got no recorded answer, read only at the set's end"
		for _, c := range calls {
			if !c.MadeInTest() {
				message = "a call read only at the set's end got no recorded answer"
			}
		}
		for _, c := range calls {
			l := strings.TrimSpace(c.Protocol + " " + c.ActualSummary)
			if c.Destination != "" {
				l += " to " + c.Destination
			}
			lines = append(lines, unclaimedWhere(c)+l)
		}
		suite.Tests++
		suite.Failures++
		suite.Cases = append(suite.Cases, junitTestCase{
			Name:      junitUnclaimedCase,
			Classname: name,
			Time:      fmtSeconds(0),
			Failure: &junitFailure{
				Message: message,
				Type:    "UnclaimedCall",
				Text:    strings.Join(lines, "\n"),
			},
		})
	}
	// A set whose status fails the run (it failed, its app halted or
	// faulted, keploy erred or a script faulted part-way, it had no tests to
	// run) is never a green suite, with the switch on or off: when no case of
	// it failed or is in error (the cases it ran passed, the rest never ran),
	// a case of its own, an <error>, says the set did not pass, with its
	// reason. Such a suite read green while the run exited non-zero.
	if setFailsRun(rep.Status) && suite.Failures == 0 && suite.Errors == 0 && !explained {
		suite.Tests++
		suite.Errors++
		suite.Cases = append(suite.Cases, junitTestCase{
			Name:      junitVerdictCase,
			Classname: name,
			Time:      fmtSeconds(0),
			Error:     &junitError{Message: notPassedSetMessage(rep.Status), Type: "TestSetNotPassed", Text: rep.FailureReason},
		})
	}
	return suite
}

// junitUnclaimedCase is the test case a suite carries whose set's end read
// a call that no mock answered and that the agent could not place outside
// every test's window, failed with the calls (buildJUnitSuite).
const junitUnclaimedCase = "keploy calls read at the set's end"

// unclaimedWhere is where c, a call the set's end read, was made, from its
// Kind: in the last test's window, outside every test's window, or at a
// time keploy cannot place (no Kind: the agent did not serve exactly).
func unclaimedWhere(c models.UnmatchedCall) string {
	switch {
	case c.MadeInTest():
		return "made in the last test: "
	case c.Kind == models.MissOutsideTest:
		return "made outside every test: "
	}
	return "made at a time keploy cannot place: "
}

// junitCaptureCase is the test case a suite whose capture was incomplete
// carries, failed with the reason (buildJUnitSuite).
const junitCaptureCase = "keploy capture"

// junitVerdictCase is a suite's set-level verdict case: NotJudged (a set run
// under the switch that keploy could not judge) or TestSetNotPassed (a set
// that failed the run while no case of it failed). At most one per suite
// (buildJUnitSuite).
const junitVerdictCase = "keploy verdict"

// notJudgedSetMessage is its <error message>.
const notJudgedSetMessage = "not judged: keploy could not tell whether this test set passed"

// setFailsRun reports whether a test set's status fails the run it is part
// of: FAILED, APP_HALTED, APP_FAULT, INTERNAL_ERR, SCRIPT_FAULT,
// NO_TESTS_TO_RUN, and RUNNING, which a report keeps once keploy is done
// only when the set never finished (a post-script fault, a failed final
// write, a keploy killed part-way; JUnit has no state for a set still
// running). PASSED and IGNORED do not, nor USER_ABORT (a run its user stopped
// is not failed), nor none (a keploy that wrote no status), which renders as
// it did.
func setFailsRun(status string) bool {
	switch models.TestSetStatus(status) {
	case models.TestSetStatusFailed, models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp,
		models.TestSetStatusInternalErr, models.TestSetStatusFaultScript, models.TestSetStatusNoTestsToRun,
		models.TestSetStatusRunning:
		return true
	}
	return false
}

// notPassedSetMessage is the <error message> of the case a suite whose set
// did not pass carries.
func notPassedSetMessage(status string) string {
	switch models.TestSetStatus(status) {
	case models.TestSetStatusNoTestsToRun:
		return "test set NO_TESTS_TO_RUN: it had no test case to run, so it verified nothing"
	case models.TestSetStatusRunning:
		return "test set RUNNING: keploy did not finish it, so it did not pass"
	}
	return "test set " + status + ": it did not pass, though no test case of it failed"
}

// redCase reports whether a test case failed or has no verdict: a suite with
// one reads red without a case of the set's own.
func redCase(t models.TestResult) bool {
	return t.Status == models.TestStatusFailed || len(t.VerdictUnknown()) > 0
}

// junitRealCase is the test case a suite mock.real's rule 4 failed carries,
// failed with the reason (buildJUnitSuite).
const junitRealCase = "keploy mock.real"

// buildFailure is a failed case's <failure>, naming the proven model's
// evidence when the switch is on now (failureOf).
func buildFailure(t models.TestResult) *junitFailure {
	return failureOf(t, provenswitch.On())
}

// failureOf is a failed case's <failure>; proven says the set was run under
// the proven switch, whose evidence it then names.
func failureOf(t models.TestResult, proven bool) *junitFailure {
	var parts []string

	if t.Kind == models.HTTP && !t.Result.StatusCode.Normal {
		parts = append(parts, fmt.Sprintf("status: expected %d, got %d",
			t.Result.StatusCode.Expected, t.Result.StatusCode.Actual))
	}

	for _, h := range t.Result.HeadersResult {
		if !h.Normal {
			parts = append(parts, fmt.Sprintf("header %s: expected %q, got %q",
				h.Expected.Key, strings.Join(h.Expected.Value, ","), strings.Join(h.Actual.Value, ",")))
		}
	}

	for _, b := range t.Result.BodyResult {
		if !b.Normal {
			parts = append(parts, fmt.Sprintf("body mismatch (%s)", b.Type))
		}
	}

	// Template variable places the answer did not satisfy: each fails the
	// test case on its own, also where every comparison row is normal.
	parts = append(parts, templateFailureLines(t.TemplateFailures)...)

	// Dependency assertions (models.Result.DepResult). Emitted alongside the
	// body rows so a CI system consuming JUnit sees a vanished outgoing call,
	// not just a response diff. Additive: DepResult had no writer before
	// keploy-consumer-design-v2.md §7 slice 4, so this loop is a no-op for
	// every report produced before it.
	parts = append(parts, depFailureLines(t.Result.DepResult)...)

	// Under the proven switch the evidence that fails a test on its calls is
	// named: each call that got no recorded answer, and each recorded call it
	// did not make, by mock.
	if proven {
		parts = append(parts, provenEvidenceLines(t.FailureInfo)...)
		// Why a gRPC body that differs only in order failed, and the way
		// out for a map field (configure proto files).
		if t.Kind == models.GRPC_EXPORT && t.FailureInfo.Assessment != nil {
			parts = append(parts, t.FailureInfo.Assessment.Reasons...)
		}
	}

	msg := "test assertion failed"
	if refusedUnsent(t.TemplateFailures) {
		// Refused when its set loaded: nothing was sent or asserted.
		msg = "test case refused when its test set loaded, not sent"
	}
	if t.FailureInfo.Risk != "" && t.FailureInfo.Risk != models.None {
		msg = fmt.Sprintf("%s [%s-RISK]", msg, t.FailureInfo.Risk)
	}

	return &junitFailure{
		Message: msg,
		Type:    "AssertionError",
		Text:    strings.Join(parts, "\n"),
	}
}

// depNonFailureBody renders the dependency detail that hangs off a testcase
// which is NOT red — a <system-out> on a passing case, or the body of an
// OBSOLETE case's <skipped> element.
//
// SAMPLED, unlike buildFailure's list. A FAILED test's <failure> body is where
// a human goes to read the whole story, so it stays complete; these two are
// attached to cases a CI system reports as green, and the flagship shape this
// slice exists to expose makes EVERY recorded dependency of EVERY test missing
// at once. Measured on 300 PASSED tests that each lost 50 dependencies (a
// downstream service removed), the uncapped body rendered 2,160,091 bytes of
// JUnit XML against roughly 25 KB for the same run before this slice — an 85x
// artifact for a run the CI system reports as passing — while the same report's
// text block was 3,120 bytes and its summary 1,439, both bounded on purpose.
//
// Nothing is lost: the count is exact, and the full per-test rows stay in the
// report file and in `keploy report --format json`, which is where an
// exhaustive consumer should read them from anyway.
//
// Returns "" when nothing is missing, which keeps every pre-slice-4 report's
// XML byte-identical.
func depNonFailureBody(deps []models.DepResult) string {
	lines := depFailureLines(deps)
	if len(lines) == 0 {
		return ""
	}
	if len(lines) <= depNoticeTestSampleSize {
		return strings.Join(lines, "\n")
	}
	rest := len(lines) - depNoticeTestSampleSize
	return strings.Join(lines[:depNoticeTestSampleSize], "\n") +
		fmt.Sprintf("\n...and %d more dependenc%s not exercised - see the report file or `keploy report --format json`",
			rest, plural(rest, "y", "ies"))
}

// templateFailureLines is one line per template variable place a test case
// failed on, and for a test case of a test set refused whole, the set's
// refusal, which is no template variable's.
func templateFailureLines(fs []models.TemplateFailure) []string {
	lines := make([]string, 0, len(fs))
	for _, f := range fs {
		if wholeSetRefusal(f) {
			lines = append(lines, templateFailureLine(f))
			continue
		}
		lines = append(lines, "template variable "+templateFailureLine(f))
	}
	return lines
}

// wholeSetRefusal reports whether f refuses a test case's whole test set (one
// a killed keploy templatize left half-written; its category is
// TEST_SET_REFUSED), which names no placeholder, rather than a placeholder of
// the test case.
func wholeSetRefusal(f models.TemplateFailure) bool {
	return f.Refused && f.Placeholder == ""
}

// templateFailureLine is one template failure as the reports say it: where,
// the placeholder, why. A refusal of a whole test set (one a killed keploy
// templatize left half-written) names no placeholder.
func templateFailureLine(f models.TemplateFailure) string {
	if f.Placeholder == "" {
		return f.Where + ": " + f.Why
	}
	return f.Where + ": " + f.Placeholder + " " + f.Why
}

// depFailureLines renders the failed dependency assertions of a result as one
// JUnit failure line per failed DepMetaResult.
func depFailureLines(deps []models.DepResult) []string {
	var parts []string
	for _, d := range deps {
		for _, m := range d.Meta {
			if m.Normal {
				continue
			}
			key := m.Key
			if key == "" {
				key = models.DepKeyPresence
			}
			parts = append(parts, fmt.Sprintf("dependency %s [%s] %s: expected %q, got %q",
				d.Name, d.Type, key, m.Expected, m.Actual))
		}
	}
	return parts
}

// ignoredSkip is the <skipped> element of an IGNORED case. A case keploy
// skipped with a reason (a quarantine) says so in the message, which is what
// every CI dashboard shows for a skipped case: "quarantined by keploy
// (<reason>)", with the reason in the body too. A case skipped without one
// keeps the historical literal, byte for byte.
func ignoredSkip(t models.TestResult) *junitSkipped {
	if t.SkipReason == "" {
		return &junitSkipped{Message: ignoredSkipMessage}
	}
	return &junitSkipped{
		Message: quarantinedSkipMessage(t.SkipReason),
		Text:    t.SkipReason,
	}
}

// quarantinedSkipMessage is the <skipped message> of a case keploy skipped for
// a reason: the reason on one line, cut at maxSkipMessageReason bytes (at a
// character's start, marked "…").
func quarantinedSkipMessage(reason string) string {
	return quarantinedLine(capBytes(models.SkipReasonOneLine(reason), maxSkipMessageReason))
}

// quarantinedLine says a case was quarantined, and why.
func quarantinedLine(reason string) string {
	return "quarantined by keploy (" + reason + ")"
}

// maxSkipMessageReason bounds the reason in a <skipped message>: a
// developer's own reason is at most 200 bytes, and a class/reason with its
// detail fits as well.
const maxSkipMessageReason = 200

// capBytes is s cut to at most n bytes at the start of a character, with "…"
// when it was cut.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ignoredSkipMessage is the historical <skipped message> literal for an IGNORED
// case skipped without a reason.
const ignoredSkipMessage = "ignored test case"

// obsoleteSkipMessage is the historical, FIXED <skipped message> literal for
// an OBSOLETE test. It is a consumer-visible string contract — CI dashboards
// group skipped cases by this attribute — so the dependency detail goes in the
// element body (junitSkipped.Text) instead of being appended here.
const obsoleteSkipMessage = "obsolete test case"

// fmtSeconds formats a duration as seconds with 3 decimal places (JUnit convention).
func fmtSeconds(d time.Duration) string {
	return fmt.Sprintf("%.3f", d.Seconds())
}

// fmtTestTime parses a time string and formats it as seconds for JUnit.
func fmtTestTime(timeTaken string) string {
	if timeTaken == "" {
		return "0.000"
	}
	dur, err := parseTimeString(timeTaken)
	if err != nil {
		return "0.000"
	}
	return fmtSeconds(dur)
}

// provenEvidenceLines are what keploy's agent could not capture while a failed
// test or one before it ran, its calls that got no recorded answer (DNS and
// connections that sent nothing left out, as the verdict leaves them out:
// models.UnmatchedCall.CountsAgainstTest) and its unconsumed test mocks, one
// line each, under the proven switch. A call is named by its shape
// (models.MissShape), and a capture loss by its kind and destination
// (models.CaptureLosses.Text, which shapes any path in it), as the proven
// model's evidence names them: the JUnit file is a CI artifact, and a call's
// URL or statement can hold the values the app sent (an email, an ID, a
// token).
func provenEvidenceLines(fi models.FailureInfo) []string {
	var lines []string
	if t := fi.CaptureIncomplete.Text(); t != "" {
		// First: it says why the rest of the evidence may be short.
		lines = append(lines, t)
	}
	for _, c := range fi.UnmatchedCalls {
		if !c.CountsAgainstTest() {
			continue
		}
		line := "call with no recorded answer: " + c.Protocol
		if shape := models.MissShape(c); shape != "" {
			line += " " + shape
		}
		if c.Destination != "" {
			line += " (to " + c.Destination + ")"
		}
		if c.Repeat != nil {
			line += "; " + c.Repeat.Brief(false)
		}
		lines = append(lines, line)
	}
	for _, m := range fi.UnconsumedMocks {
		line := fmt.Sprintf("recorded call not made: mock %s", m.Name)
		if m.Kind != "" {
			line += " (" + m.Kind + ")"
		}
		if m.ConsumedBy != "" {
			line += "; " + m.ConsumedBy + " consumed it, and the order the shared mock pool served the two in can decide which test it counts for"
		}
		if m.RepeatOf != nil {
			line += "; " + m.RepeatOf.Brief(true)
		}
		lines = append(lines, line)
	}
	return lines
}
