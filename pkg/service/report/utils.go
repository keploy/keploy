package report

import (
	"bufio"
	"fmt"
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
func printSingleSummaryTo(w *bufio.Writer, name string, total, pass, fail int, dur time.Duration, failedCases []string) {
	// Define Colors
	const (
		reset = "\x1b[0m"
		blue  = "\x1b[34;1m" // Blue and Bold
		red   = "\x1b[31;1m" // Red and Bold
	)

	// Format the duration string
	timeStr := "N/A"
	if dur > 0 {
		// Use %s, not %q here to avoid quoted/escaped output
		timeStr = fmtDuration(dur) 
	}

	// 1. HEADER
	fmt.Fprintln(w, "<=========================================>")
	fmt.Fprintln(w, " COMPLETE TESTRUN SUMMARY.3")
	
	// Note: We use %s for the values so we can inject the color codes manually.
	// We convert the integers to strings inside the Printf using color variables.
	fmt.Fprintf(w, "\tTotal tests:       %s%d%s\n", blue, total, reset)
	fmt.Fprintf(w, "\tTotal test passed: %s%d%s\n", blue, pass, reset)
	fmt.Fprintf(w, "\tTotal test failed: %s%d%s\n", blue, fail, reset)
	
	// FIX: Use %s for the time string, wrapped in Red color
	fmt.Fprintf(w, "\tTotal time taken:  %s%s%s\n", red, timeStr, reset)

	// 2. TABLE
	// Use tabwriter for alignment
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\tTest Suite\tTotal\tPassed\tFailed\tTime Taken")
	
	// We formatting the row with %s (string) for name and time, %d (int) for numbers
	// If you want color in the table too, apply it to the individual args
	fmt.Fprintf(tw, "\t%s\t%d\t%d\t%d\t%s\n", name, total, pass, fail, timeStr)
	tw.Flush()

	// 3. FAILURES
	fmt.Fprintln(w, "\nFAILED TEST CASES:")
	if fail == 0 || len(failedCases) == 0 {
		fmt.Fprintln(w, "\t(none)")
	} else {
		for _, fc := range failedCases {
			// Using %s prevents quotes around the test names
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

	// ANSI Constants
	const (
		reset  = "\x1b[0m"
		yellow = "\x1b[33m"
		red    = "\x1b[31m"
		green  = "\x1b[32m"
	)

	var sb strings.Builder
	// Pre-allocate to avoid resizing. length of diff + some buffer for color codes
	sb.Grow(len(diff) + 100) 

	scanner := bufio.NewScanner(strings.NewReader(diff))
	first := true

	for scanner.Scan() {
		line := scanner.Text()
		
		// Manage newlines manually to avoid trailing newline issues
		if !first {
			sb.WriteByte('\n')
		}
		first = false

		// Identify logic based on prefixes
		// We use a switch with simple logic for readability
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

// GenerateStatusAndHeadersTableDiff builds a table-style diff for status code, headers,
// trailer headers, and synthetic content-length when body differs and header is absent.
func GenerateStatusAndHeadersTableDiff(test models.TestResult) string {
	var sb strings.Builder
	sb.WriteString("=== CHANGES IN STATUS AND HEADERS ===\n")

	hasDiff := false

	// Status code (only for HTTP tests as grpc status is part of headers)
	if !test.Result.StatusCode.Normal && test.Kind == models.HTTP {
		hasDiff = true
		sb.WriteString("Path: status_code\n")
		sb.WriteString(fmt.Sprintf("  Expected: %d\n", test.Result.StatusCode.Expected))
		sb.WriteString(fmt.Sprintf("  Actual: %d\n\n", test.Result.StatusCode.Actual))
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
