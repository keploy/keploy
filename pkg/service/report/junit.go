package report

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

type junitTestSuites struct {
	XMLName  xml.Name         `xml:"testsuites"`
	Tests    int              `xml:"tests,attr"`
	Failures int              `xml:"failures,attr"`
	Time     string           `xml:"time,attr"`
	Suites   []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	Name     string          `xml:"name,attr"`
	Tests    int             `xml:"tests,attr"`
	Failures int             `xml:"failures,attr"`
	Skipped  int             `xml:"skipped,attr"`
	Time     string          `xml:"time,attr"`
	Cases    []junitTestCase `xml:"testcase"`
}

type junitTestCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
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

type junitSkipped struct {
	Message string `xml:"message,attr,omitempty"`
	// Text is the element body. Detail belongs HERE, not appended to the
	// message attribute: the attribute is what Jenkins/GitLab/Buildkite group
	// and display skipped cases by, so varying it per test would explode a
	// single "obsolete test case" bucket into one bucket per dependency
	// combination, and an unbounded attribute (one clause per unconsumed mock)
	// would put kilobytes on one XML line.
	Text string `xml:",chardata"`
}

// generateJUnit writes JUnit XML output for the collected test reports.
func (r *Report) generateJUnit(reports map[string]*models.TestReport) error {
	suites := buildJUnitSuites(reports)
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
	var totalTests, totalFailures int
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
		suite := buildJUnitSuite(name, rep)
		totalTests += suite.Tests
		totalFailures += suite.Failures
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
		Time:     fmtSeconds(totalDuration),
		Suites:   suites,
	}
}

func buildJUnitSuite(name string, rep *models.TestReport) junitTestSuite {
	cases := make([]junitTestCase, 0, len(rep.Tests))
	var failures, skipped int

	for _, t := range rep.Tests {
		tc := junitTestCase{
			Name:      t.TestCaseID,
			Classname: name,
			Time:      fmtTestTime(t.TimeTaken),
		}

		switch t.Status {
		case models.TestStatusFailed:
			failures++
			tc.Failure = buildFailure(t)
		case models.TestStatusObsolete:
			skipped++
			tc.Skipped = &junitSkipped{
				Message: obsoleteSkipMessage,
				Text:    depNonFailureBody(t.Result.DepResult),
			}
		case models.TestStatusIgnored:
			skipped++
			tc.Skipped = &junitSkipped{Message: "ignored test case"}
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

	return junitTestSuite{
		Name:     name,
		Tests:    rep.Total,
		Failures: failures,
		Skipped:  skipped,
		Time:     fmtSeconds(suiteDur),
		Cases:    cases,
	}
}

func buildFailure(t models.TestResult) *junitFailure {
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

	// Dependency assertions (models.Result.DepResult). Emitted alongside the
	// body rows so a CI system consuming JUnit sees a vanished outgoing call,
	// not just a response diff. Additive: DepResult had no writer before
	// keploy-consumer-design-v2.md §7 slice 4, so this loop is a no-op for
	// every report produced before it.
	parts = append(parts, depFailureLines(t.Result.DepResult)...)

	msg := "test assertion failed"
	if t.FailureInfo.Risk != "" && t.FailureInfo.Risk != models.None {
		msg = fmt.Sprintf("test assertion failed [%s-RISK]", t.FailureInfo.Risk)
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
			// "dependency" is the sync path's noun and is what every
			// pre-consumer report says; an effects[i] row is an assertion
			// about what the worker PRODUCED, not about a call it made, so
			// calling it a dependency would misdescribe it. Dispatched on the
			// row prefix, never on the test's Kind.
			noun := "dependency"
			if models.IsEffectRow(d) {
				noun = "effect"
			}
			parts = append(parts, fmt.Sprintf("%s %s [%s] %s: expected %q, got %q",
				noun, d.Name, d.Type, key, m.Expected, m.Actual))
		}
	}
	return parts
}

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
