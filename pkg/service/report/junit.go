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
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr,omitempty"`
	Text    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr,omitempty"`
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
			tc.Skipped = &junitSkipped{Message: "obsolete test case"}
		case models.TestStatusIgnored:
			skipped++
			tc.Skipped = &junitSkipped{Message: "ignored test case"}
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
