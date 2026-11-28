package report

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// estimateDuration tries to compute sum of TimeTaken across tests if those fields exist.
func estimateDuration(tests []models.TestResult) time.Duration {
	var sum time.Duration
	for _, t := range tests {
		if t.TimeTaken != "" {
			if dur, err := parseTimeString(t.TimeTaken); err == nil {
				sum += dur
			}
		}
	}
	return sum
}

// parseTimeString parses time strings in formats like "1.5s", "2m30s", etc.
func parseTimeString(timeStr string) (time.Duration, error) {
	return time.ParseDuration(timeStr)
}

func fmtDuration(d time.Duration) string {
	// 28.54 s style
	secs := float64(d) / float64(time.Second)
	return fmt.Sprintf("%.2f s", secs)
}

// printSingleSummaryTo is the buffered variant used internally.
// ignored is the tests counted as skipped, not passed (only under the proven
// switch); a line says it when there are any.
func printSingleSummaryTo(w *bufio.Writer, name string, total, pass, fail, obsolete, ignored int, dur time.Duration, failedCases []string) {
	if models.IsAnsiDisabled {
		fmt.Fprintln(w, "<=========================================>")
		fmt.Fprintln(w, " COMPLETE TESTRUN SUMMARY.")
		fmt.Fprintf(w, "\tTotal tests: %d\n", total)
		fmt.Fprintf(w, "\tTotal test passed: %d\n", pass)
		fmt.Fprintf(w, "\tTotal test failed: %d\n", fail)
		if obsolete > 0 {
			fmt.Fprintf(w, "\tTotal test obsolete: %d\n", obsolete)
		}
		if ignored > 0 {
			fmt.Fprintf(w, "\tTotal test ignored: %d\n", ignored)
		}
		if dur > 0 {
			fmt.Fprintf(w, "\tTotal time taken: %q\n", fmtDuration(dur))
		} else {
			fmt.Fprintf(w, "\tTotal time taken: %q\n", "N/A")
		}
		header := "\tTest Suite\t\tTotal\tPassed\t\tFailed"
		if obsolete > 0 {
			header += "\t\tObsolete"
		}
		header += "\t\tTime Taken\t"
		fmt.Fprintln(w, header)
		tt := "N/A"
		if dur > 0 {
			tt = fmtDuration(dur)
		}

		if obsolete > 0 {
			fmt.Fprintf(w, "\t%q\t\t%d\t\t%d\t\t%d\t\t%d\t\t%q\n", name, total, pass, fail, obsolete, tt)
		} else {
			fmt.Fprintf(w, "\t%q\t\t%d\t\t%d\t\t%d\t\t%q\n", name, total, pass, fail, tt)
		}

		fmt.Fprintln(w, "\nFAILED TEST CASES:")
		if fail == 0 || len(failedCases) == 0 {
			fmt.Fprintln(w, "\t(none)")
		} else {
			for _, fc := range failedCases {
				fmt.Fprintf(w, "\t- %s\n", fc)
			}
		}
		fmt.Fprintln(w, "<=========================================>")
		return
	}

	const (
		reset = "\x1b[0m"
		blue  = "\x1b[34;1m" // Blue and Bold
		red   = "\x1b[31;1m" // Red and Bold
	)

	timeStr := "N/A"
	if dur > 0 {
		timeStr = fmtDuration(dur)
	}

	fmt.Fprintln(w, "<=========================================>")
	fmt.Fprintln(w, " COMPLETE TESTRUN SUMMARY.")

	fmt.Fprintf(w, "\tTotal tests:       %s%d%s\n", blue, total, reset)
	fmt.Fprintf(w, "\tTotal test passed: %s%d%s\n", blue, pass, reset)
	fmt.Fprintf(w, "\tTotal test failed: %s%d%s\n", blue, fail, reset)
	if obsolete > 0 {
		fmt.Fprintf(w, "\tTotal test obsolete: %s%d%s\n", blue, obsolete, reset)
	}
	if ignored > 0 {
		fmt.Fprintf(w, "\tTotal test ignored: %s%d%s\n", blue, ignored, reset)
	}

	fmt.Fprintf(w, "\tTotal time taken:  %s%s%s\n", red, timeStr, reset)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := "\tTest Suite\tTotal\tPassed\tFailed"
	if obsolete > 0 {
		header += "\tObsolete"
	}
	header += "\tTime Taken"
	fmt.Fprintln(tw, header)

	if obsolete > 0 {
		fmt.Fprintf(tw, "\t%s\t%d\t%d\t%d\t%d\t%s\n", name, total, pass, fail, obsolete, timeStr)
	} else {
		fmt.Fprintf(tw, "\t%s\t%d\t%d\t%d\t%s\n", name, total, pass, fail, timeStr)
	}
	tw.Flush()

	fmt.Fprintln(w, "\nFAILED TEST CASES:")
	if fail == 0 || len(failedCases) == 0 {
		fmt.Fprintln(w, "\t(none)")
	} else {
		for _, fc := range failedCases {
			fmt.Fprintf(w, "\t- %s\n", fc)
		}
	}
	fmt.Fprintln(w, "<=========================================>")
}

// applyCliColorsToDiff adds ANSI colors to values in the JSON diff block.
// - Value after "Path:" is yellow
// - Value after "Expected:" is red
// - Value after "Actual:" is green
func applyCliColorsToDiff(diff string) string {
	if diff == "" {
		return ""
	}

	if models.IsAnsiDisabled {
		return diff
	}

	// ANSI Constants
	const (
		reset  = "\x1b[0m"
		yellow = "\x1b[33m"
		red    = "\x1b[31m"
		green  = "\x1b[32m"
	)

	var sb strings.Builder
	sb.Grow(len(diff) + 100)

	scanner := bufio.NewScanner(strings.NewReader(diff))
	first := true

	for scanner.Scan() {
		line := scanner.Text()

		if !first {
			sb.WriteByte('\n')
		}
		first = false

		switch {
		case strings.HasPrefix(line, "Path: "):
			val := strings.TrimPrefix(line, "Path: ")
			sb.WriteString("Path: " + yellow + val + reset)

		case strings.HasPrefix(line, "  Expected: "):
			val := strings.TrimPrefix(line, "  Expected: ")
			sb.WriteString("  Expected: " + red + val + reset)

		case strings.HasPrefix(line, "  Actual: "):
			val := strings.TrimPrefix(line, "  Actual: ")
			sb.WriteString("  Actual: " + green + val + reset)

		default:
			sb.WriteString(line)
		}
	}

	return sb.String()
}

// grpcStatusNames are the gRPC status codes' canonical names (grpc/grpc
// doc/statuscodes.md), as a gRPC user reads them.
var grpcStatusNames = []string{
	"OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND",
	"ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION",
	"ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS",
	"UNAUTHENTICATED",
}

// GrpcStatusText is a grpc-status as a report shows it: the code with its
// name ("13 (INTERNAL)"), the bare code for one outside the defined set, and
// "none or not a number" for the gRPC matcher's -1: no grpc-status it could
// read (absent, or not a number).
func GrpcStatusText(code int) string {
	if code == -1 {
		return "none or not a number"
	}
	if code < 0 {
		return fmt.Sprintf("%d", code)
	}
	if code < len(grpcStatusNames) {
		return fmt.Sprintf("%d (%s)", code, grpcStatusNames[code])
	}
	return fmt.Sprintf("%d", code)
}

// statusChangeShown says whether a test's status is shown as changed: it is
// a difference (a status assertion failed, or the recorded and replayed
// statuses differ).
func statusChangeShown(test models.TestResult) bool {
	return !test.Result.StatusCode.Normal
}

// statusTexts are a test's expected and actual status as reports show them:
// what a failed status assertion asked for when it named no single status
// (IntResult.ExpectedText), a gRPC status by code and name.
func statusTexts(test models.TestResult) (expected, actual string) {
	sc := test.Result.StatusCode
	if test.Kind == models.GRPC_EXPORT {
		expected, actual = GrpcStatusText(sc.Expected), GrpcStatusText(sc.Actual)
	} else {
		expected, actual = fmt.Sprint(sc.Expected), fmt.Sprint(sc.Actual)
	}
	if sc.ExpectedText != "" {
		expected = sc.ExpectedText
		// A gRPC status asked for by code reads as one recorded does.
		if code, err := strconv.Atoi(sc.ExpectedText); err == nil && test.Kind == models.GRPC_EXPORT {
			expected = GrpcStatusText(code)
		}
	}
	return expected, actual
}

// GenerateStatusAndHeadersTableDiff builds a table-style diff for status code, headers,
// trailer headers, and synthetic content-length when body differs and header is absent.
func GenerateStatusAndHeadersTableDiff(test models.TestResult) string {
	var sb strings.Builder
	sb.WriteString("=== CHANGES IN STATUS AND HEADERS ===\n")

	hasDiff := false

	// Status code: an HTTP status, or a gRPC test's grpc-status (the gRPC
	// matcher keeps it here, not in TrailerResult).
	if statusChangeShown(test) && (test.Kind == models.HTTP || test.Kind == models.GRPC_EXPORT) {
		hasDiff = true
		path := "status_code"
		if test.Kind == models.GRPC_EXPORT {
			path = "trailer.grpc-status"
		}
		expected, actual := statusTexts(test)
		sb.WriteString("Path: " + path + "\n")
		fmt.Fprintf(&sb, "  Expected: %s\n", expected)
		fmt.Fprintf(&sb, "  Actual: %s\n\n", actual)
	}

	// Headers
	for _, hr := range test.Result.HeadersResult {
		if hr.Normal {
			continue
		}
		hasDiff = true
		expected := strings.Join(hr.Expected.Value, ", ")
		actual := strings.Join(hr.Actual.Value, ", ")
		sb.WriteString(fmt.Sprintf("Path: header.%s\n", hr.Actual.Key))
		sb.WriteString(fmt.Sprintf("  Expected: %s\n", expected))
		sb.WriteString(fmt.Sprintf("  Actual: %s\n\n", actual))
	}

	// Trailer headers
	for _, tr := range test.Result.TrailerResult {
		if tr.Normal {
			continue
		}
		hasDiff = true
		expected := strings.Join(tr.Expected.Value, ", ")
		actual := strings.Join(tr.Actual.Value, ", ")
		sb.WriteString(fmt.Sprintf("Path: trailer.%s\n", tr.Actual.Key))
		sb.WriteString(fmt.Sprintf("  Expected: %s\n", expected))
		sb.WriteString(fmt.Sprintf("  Actual: %s\n\n", actual))
	}

	// Synthetic content length if body differs and content-length header wasn't already reported
	hasContentLengthHeaderChange := false
	for _, hr := range test.Result.HeadersResult {
		if strings.EqualFold(hr.Actual.Key, "Content-Length") || strings.EqualFold(hr.Expected.Key, "Content-Length") {
			hasContentLengthHeaderChange = !hr.Normal
			break
		}
	}
	if !hasContentLengthHeaderChange {
		for _, br := range test.Result.BodyResult {
			if br.Normal {
				continue
			}
			expLen := len(br.Expected)
			actLen := len(br.Actual)
			if expLen != actLen {
				hasDiff = true
				sb.WriteString("Path: content_length\n")
				sb.WriteString(fmt.Sprintf("  Expected: %d\n", expLen))
				sb.WriteString(fmt.Sprintf("  Actual: %d\n\n", actLen))
				break
			}
		}
	}

	if !hasDiff {
		return "No differences found in status or headers."
	}
	return strings.TrimSpace(sb.String())
}
