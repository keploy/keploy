package schemasummary

import (
	"fmt"
	"strings"
	"time"
)

// Render formats a Report as the box-drawn ASCII table the CLI prints.
// Pure function — no I/O, no globals — so it's trivially unit-testable.
func Render(r Report) string {
	const sep = "────────────────────────────────"
	icon := map[Status]string{
		StatusCovered:   "✔",
		StatusPartial:   "◐",
		StatusUncovered: "✘",
	}

	var b strings.Builder
	fmt.Fprintln(&b, "API Schema Coverage")
	fmt.Fprintln(&b, sep)

	fmt.Fprintf(&b, "Cluster      : %s\n", r.Cluster)
	fmt.Fprintf(&b, "Namespace    : %s\n", r.Namespace)
	fmt.Fprintf(&b, "Deployment   : %s\n", r.Deployment)
	if r.AppRelease != "" {
		fmt.Fprintf(&b, "Release      : %s\n", r.AppRelease)
	}
	if r.LastUpdated > 0 {
		fmt.Fprintf(&b, "Last Updated : %s\n", time.Unix(r.LastUpdated, 0).Format("2006-01-02 15:04:05"))
	}

	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Coverage     : %.1f%% (%d/%d endpoints)\n",
		r.CoveragePercentage, r.CoveredEndpoints, r.TotalEndpoints)

	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Status Breakdown")
	fmt.Fprintf(&b, "%s Covered   : %d\n", icon[StatusCovered], r.StatusBreakdown.Covered)
	fmt.Fprintf(&b, "%s Partial   : %d\n", icon[StatusPartial], r.StatusBreakdown.Partial)
	fmt.Fprintf(&b, "%s Uncovered : %d\n", icon[StatusUncovered], r.StatusBreakdown.Uncovered)

	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Endpoints")
	fmt.Fprintln(&b, sep)
	if len(r.Endpoints) == 0 {
		fmt.Fprintln(&b, "(none)")
	}
	for _, e := range r.Endpoints {
		fmt.Fprintf(&b, "%s %-6s %s\n", icon[e.Status], strings.ToUpper(e.Method), e.Path)
	}

	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Schemas")
	fmt.Fprintln(&b, sep)
	if len(r.Schemas) == 0 {
		fmt.Fprintln(&b, "(none)")
	}
	for _, c := range r.Schemas {
		fmt.Fprintf(&b, "%s %s\n", icon[c.Status], c.Name)
	}

	return b.String()
}
