package report

import (
	"fmt"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
)

func filterTestsByIDs(tests []models.TestResult, ids []string) []models.TestResult {
	set := map[string]struct{}{}
	for _, id := range ids {
		set[strings.TrimSpace(id)] = struct{}{}
	}
	out := make([]models.TestResult, 0, len(ids))
	for _, t := range tests {
		if _, ok := set[t.TestCaseID]; ok {
			out = append(out, t)
		}
	}
	return out
}

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

// printSingleSummary prints a compact summary for a single report source
func printSingleSummary(name string, total, pass, fail int, dur time.Duration, failedCases []string) {
	fmt.Println("<=========================================>")
	fmt.Println(" COMPLETE TESTRUN SUMMARY.")
	fmt.Printf("\tTotal tests: %d\n", total)
	fmt.Printf("\tTotal test passed: %d\n", pass)
	fmt.Printf("\tTotal test failed: %d\n", fail)
	if dur > 0 {
		fmt.Printf("\tTotal time taken: %q\n", fmtDuration(dur))
	} else {
		fmt.Printf("\tTotal time taken: %q\n", "N/A")
	}
	fmt.Println("\tTest Suite\t\tTotal\tPassed\t\tFailed\t\tTime Taken\t")
	tt := "N/A"
	if dur > 0 {
		tt = fmtDuration(dur)
	}
	fmt.Printf("\t%q\t\t%d\t\t%d\t\t%d\t\t%q\n", name, total, pass, fail, tt)

	fmt.Println("\nFAILED TEST CASES:")
	if fail == 0 || len(failedCases) == 0 {
		fmt.Println("\t(none)")
	} else {
		for _, fc := range failedCases {
			fmt.Printf("\t- %s\n", fc)
		}
	}
	fmt.Println("<=========================================>")
}

// applyCliColorsToDiff adds ANSI colors to values in the JSON diff block.
// - Value after "Path:" is yellow
// - Value after "Old:" is red
// - Value after "New:" is green
func applyCliColorsToDiff(diff string) string {
	const (
		ansiReset  = "\x1b[0m"
		ansiYellow = "\x1b[33m"
		ansiRed    = "\x1b[31m"
		ansiGreen  = "\x1b[32m"
	)

	lines := strings.Split(diff, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "Path: ") {
			// Color only the value after "Path: " in yellow
			value := strings.TrimPrefix(line, "Path: ")
			lines[i] = "Path: " + ansiYellow + value + ansiReset
			continue
		}
		if strings.HasPrefix(line, "  Old: ") {
			// Color only the value after "  Old: " in red
			value := strings.TrimPrefix(line, "  Old: ")
			lines[i] = "  Old: " + ansiRed + value + ansiReset
			continue
		}
		if strings.HasPrefix(line, "  New: ") {
			// Color only the value after "  New: " in green
			value := strings.TrimPrefix(line, "  New: ")
			lines[i] = "  New: " + ansiGreen + value + ansiReset
			continue
		}
	}
	return strings.Join(lines, "\n")
}

// GenerateStatusAndHeadersTableDiff builds a table-style diff for status code, headers,
// trailer headers, and synthetic content-length when body differs and header is absent.
func GenerateStatusAndHeadersTableDiff(test models.TestResult) string {
	var sb strings.Builder
	sb.WriteString("=== CHANGES IN STATUS AND HEADERS ===\n")

	hasDiff := false

	// Status code
	if !test.Result.StatusCode.Normal {
		hasDiff = true
		sb.WriteString("Path: status_code\n")
		sb.WriteString(fmt.Sprintf("  Old: %d\n", test.Result.StatusCode.Expected))
		sb.WriteString(fmt.Sprintf("  New: %d\n\n", test.Result.StatusCode.Actual))
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
		sb.WriteString(fmt.Sprintf("  Old: %s\n", expected))
		sb.WriteString(fmt.Sprintf("  New: %s\n\n", actual))
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
		sb.WriteString(fmt.Sprintf("  Old: %s\n", expected))
		sb.WriteString(fmt.Sprintf("  New: %s\n\n", actual))
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
				sb.WriteString(fmt.Sprintf("  Old: %d\n", expLen))
				sb.WriteString(fmt.Sprintf("  New: %d\n\n", actLen))
				break
			}
		}
	}

	if !hasDiff {
		return "No differences found in status or headers."
	}
	return strings.TrimSpace(sb.String())
}
