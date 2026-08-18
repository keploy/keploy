// Mismatch-report diagnostics for the MySQL replayer: the data matchCommand
// hands back on a "no matching mock" outcome and the formatting helpers
// simulateCommandPhase uses to render it into a models.MockMismatchReport.
package replayer

import (
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// mockMiss carries the diagnostics of a "no matching mock" outcome so the
// caller can build a meaningful mismatch report: the closest candidate (name +
// its recorded query), field-level diffs against the first strict-rejected
// candidate (noise-vocabulary "body."-paths with recorded/live values), and
// how many candidates strict enforcement ruled out.
type mockMiss struct {
	closestQuery   string
	closestMock    string
	fieldDiffs     []models.MockFieldDiff
	strictRejected int
}

// formatExecParams renders bound parameter values for mismatch reports,
// bounded per-value and overall so a blob param can't flood the report.
func formatExecParams(params []mysql.Parameter) string {
	if len(params) == 0 {
		return ""
	}
	vals := make([]string, 0, len(params))
	for _, p := range params {
		vals = append(vals, truncate(fmt.Sprintf("%v", p.Value), 48))
	}
	return "params=[" + strings.Join(vals, ", ") + "]"
}
