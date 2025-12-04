package coverage

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

// Reporter generates coverage reports in various formats.
type Reporter struct {
	stats *CoverageStats
}

// NewReporter creates a new reporter for the given statistics.
func NewReporter(stats *CoverageStats) *Reporter {
	return &Reporter{stats: stats}
}

// ToJSON returns coverage statistics as formatted JSON.
func (r *Reporter) ToJSON() (string, error) {
	data, err := json.MarshalIndent(r.stats, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal coverage stats to JSON: %w", err)
	}
	return string(data), nil
}

// ToText returns coverage statistics as a human-readable text summary.
func (r *Reporter) ToText() string {
	var sb strings.Builder

	sb.WriteString("╔═══════════════════════════════════════════════════════════╗\n")
	sb.WriteString("║            MOCK REPLAY COVERAGE REPORT                    ║\n")
	sb.WriteString("╚═══════════════════════════════════════════════════════════╝\n\n")

	// Summary section
	sb.WriteString(fmt.Sprintf("📊 Overall Coverage: %.2f%% (%d/%d mocks)\n",
		r.stats.CoveragePercent, r.stats.ReplayedMocks, r.stats.TotalMocks))
	sb.WriteString(fmt.Sprintf("✅ Used Mocks: %d\n", r.stats.ReplayedMocks))
	sb.WriteString(fmt.Sprintf("❌ Missed Mocks: %d\n", r.stats.MissedMocks))

	if r.stats.TestRunID != "" {
		sb.WriteString(fmt.Sprintf("🔧 Test Run ID: %s\n", r.stats.TestRunID))
	}
	if r.stats.TestSetID != "" {
		sb.WriteString(fmt.Sprintf("📦 Test Set ID: %s\n", r.stats.TestSetID))
	}
	sb.WriteString(fmt.Sprintf("⏱️  Timestamp: %s\n\n", r.stats.Timestamp.Format("2006-01-02 15:04:05")))

	// Endpoint breakdown section
	if len(r.stats.Endpoints) > 0 {
		sb.WriteString("📋 Endpoint Coverage Breakdown:\n")
		sb.WriteString("─────────────────────────────────────────────────────────────\n")

		// Create a table writer
		w := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "METHOD\tPATH\tCOVERAGE\tUSED/TOTAL")
		fmt.Fprintln(w, "───────\t─────\t────────\t──────────")

		// Sort endpoints by method, then path for consistent output
		var sortedKeys []string
		for key := range r.stats.Endpoints {
			sortedKeys = append(sortedKeys, key)
		}
		// Simple alphabetic sort; in production could use more sophisticated sorting
		for _, key := range sortedKeys {
			endpoint := r.stats.Endpoints[key]
			coverageStr := fmt.Sprintf("%.1f%%", endpoint.CoveragePercent)
			countStr := fmt.Sprintf("%d/%d", endpoint.Replayed, endpoint.Total)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", endpoint.Method, endpoint.Path, coverageStr, countStr)
		}
		w.Flush()
		sb.WriteString("─────────────────────────────────────────────────────────────\n\n")
	}

	// Missed mocks section
	if len(r.stats.MissedMockIDs) > 0 {
		sb.WriteString(fmt.Sprintf("⚠️  Missed Mocks (%d):\n", len(r.stats.MissedMockIDs)))
		for _, mockID := range r.stats.MissedMockIDs {
			metadata := r.getMockMetadata(mockID)
			if metadata != nil {
				sb.WriteString(fmt.Sprintf("  • [%s] %s %s\n", mockID, metadata.Method, metadata.Path))
			} else {
				sb.WriteString(fmt.Sprintf("  • [%s] (unknown)\n", mockID))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// ToHTML returns coverage statistics as an HTML report.
func (r *Reporter) ToHTML() string {
	var sb strings.Builder

	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Mock Replay Coverage Report</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, Cantarell, sans-serif;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            min-height: 100vh;
            padding: 20px;
        }
        .container {
            max-width: 1000px;
            margin: 0 auto;
            background: white;
            border-radius: 8px;
            box-shadow: 0 20px 60px rgba(0, 0, 0, 0.3);
            overflow: hidden;
        }
        header {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            padding: 40px 20px;
            text-align: center;
        }
        header h1 {
            font-size: 2.5em;
            margin-bottom: 10px;
        }
        .summary {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 20px;
            padding: 30px;
            background: #f8f9fa;
        }
        .stat-card {
            background: white;
            padding: 20px;
            border-radius: 8px;
            border-left: 4px solid #667eea;
            box-shadow: 0 2px 8px rgba(0, 0, 0, 0.1);
        }
        .stat-card h3 {
            color: #666;
            font-size: 0.9em;
            font-weight: 600;
            text-transform: uppercase;
            margin-bottom: 10px;
            letter-spacing: 1px;
        }
        .stat-card .value {
            font-size: 2em;
            font-weight: bold;
            color: #333;
        }
        .coverage-bar {
            margin-top: 15px;
            background: #e9ecef;
            height: 10px;
            border-radius: 5px;
            overflow: hidden;
        }
        .coverage-fill {
            height: 100%;
            background: linear-gradient(90deg, #667eea 0%, #764ba2 100%);
            border-radius: 5px;
            transition: width 0.3s ease;
        }
        table {
            width: 100%;
            border-collapse: collapse;
            margin: 20px 0;
        }
        th {
            background: #f8f9fa;
            padding: 12px;
            text-align: left;
            font-weight: 600;
            color: #333;
            border-bottom: 2px solid #dee2e6;
        }
        td {
            padding: 12px;
            border-bottom: 1px solid #dee2e6;
        }
        tr:hover { background: #f8f9fa; }
        .endpoints-section,
        .missed-section {
            padding: 30px;
            border-top: 1px solid #dee2e6;
        }
        h2 {
            color: #333;
            margin-bottom: 20px;
            font-size: 1.5em;
        }
        .method-badge {
            display: inline-block;
            padding: 4px 8px;
            border-radius: 4px;
            font-weight: 600;
            font-size: 0.85em;
            color: white;
        }
        .method-get { background: #17a2b8; }
        .method-post { background: #28a745; }
        .method-put { background: #ffc107; color: #333; }
        .method-delete { background: #dc3545; }
        .method-patch { background: #6f42c1; }
        .missed-list {
            list-style: none;
            padding: 0;
        }
        .missed-list li {
            padding: 10px;
            margin-bottom: 10px;
            background: #fff3cd;
            border-left: 4px solid #ffc107;
            border-radius: 4px;
        }
        .coverage-good { color: #28a745; }
        .coverage-warning { color: #ffc107; }
        .coverage-poor { color: #dc3545; }
        footer {
            background: #f8f9fa;
            padding: 20px;
            text-align: center;
            color: #666;
            font-size: 0.9em;
            border-top: 1px solid #dee2e6;
        }
    </style>
</head>
<body>
    <div class="container">
        <header>
            <h1>📊 Mock Replay Coverage Report</h1>
            <p>Test coverage analysis for mock replays</p>
        </header>
`)

	// Summary cards
	sb.WriteString(`        <div class="summary">
`)
	coverageClass := "coverage-good"
	if r.stats.CoveragePercent < 50 {
		coverageClass = "coverage-poor"
	} else if r.stats.CoveragePercent < 80 {
		coverageClass = "coverage-warning"
	}

	sb.WriteString(fmt.Sprintf(`            <div class="stat-card">
                <h3>Overall Coverage</h3>
                <div class="value %s">%.2f%%</div>
                <div class="coverage-bar">
                    <div class="coverage-fill" style="width: %.2f%%"></div>
                </div>
            </div>
`, coverageClass, r.stats.CoveragePercent, r.stats.CoveragePercent))

	sb.WriteString(fmt.Sprintf(`            <div class="stat-card">
                <h3>Used Mocks</h3>
                <div class="value">%d</div>
            </div>
            <div class="stat-card">
                <h3>Total Mocks</h3>
                <div class="value">%d</div>
            </div>
            <div class="stat-card">
                <h3>Missed Mocks</h3>
                <div class="value coverage-poor">%d</div>
            </div>
`, r.stats.ReplayedMocks, r.stats.TotalMocks, r.stats.MissedMocks))

	sb.WriteString(`        </div>
`)

	// Endpoint breakdown
	if len(r.stats.Endpoints) > 0 {
		sb.WriteString(`        <div class="endpoints-section">
            <h2>📋 Endpoint Coverage Breakdown</h2>
            <table>
                <thead>
                    <tr>
                        <th>Method</th>
                        <th>Path</th>
                        <th>Coverage</th>
                        <th>Used / Total</th>
                    </tr>
                </thead>
                <tbody>
`)

		for _, key := range r.getSortedEndpointKeys() {
			endpoint := r.stats.Endpoints[key]
			methodClass := fmt.Sprintf("method-%s", strings.ToLower(endpoint.Method))
			coverageClass := "coverage-good"
			if endpoint.CoveragePercent < 50 {
				coverageClass = "coverage-poor"
			} else if endpoint.CoveragePercent < 80 {
				coverageClass = "coverage-warning"
			}

			sb.WriteString(fmt.Sprintf(`                    <tr>
                        <td><span class="method-badge %s">%s</span></td>
                        <td><code>%s</code></td>
                        <td class="%s">%.1f%%</td>
                        <td>%d / %d</td>
                    </tr>
`, methodClass, endpoint.Method, endpoint.Path, coverageClass, endpoint.CoveragePercent, endpoint.Replayed, endpoint.Total))
		}

		sb.WriteString(`                </tbody>
            </table>
        </div>
`)
	}

	// Missed mocks
	if len(r.stats.MissedMockIDs) > 0 {
		sb.WriteString(fmt.Sprintf(`        <div class="missed-section">
            <h2>⚠️  Missed Mocks (%d)</h2>
            <ul class="missed-list">
`, len(r.stats.MissedMockIDs)))

		for _, mockID := range r.stats.MissedMockIDs {
			metadata := r.getMockMetadata(mockID)
			if metadata != nil {
				sb.WriteString(fmt.Sprintf(`                <li><strong>[%s]</strong> %s <code>%s</code></li>
`, mockID, metadata.Method, metadata.Path))
			} else {
				sb.WriteString(fmt.Sprintf(`                <li><strong>[%s]</strong> (unknown endpoint)</li>
`, mockID))
			}
		}

		sb.WriteString(`            </ul>
        </div>
`)
	}

	// Footer
	sb.WriteString(fmt.Sprintf(`        <footer>
            <p>Generated: %s</p>
        </footer>
    </div>
</body>
</html>
`, r.stats.Timestamp.Format("2006-01-02 15:04:05 MST")))

	return sb.String()
}

// Helper to get mock metadata
func (r *Reporter) getMockMetadata(mockID string) *MockMetadata {
	for _, endpoint := range r.stats.Endpoints {
		for _, id := range endpoint.MockIDs {
			if id == mockID {
				return &MockMetadata{
					ID:     mockID,
					Method: endpoint.Method,
					Path:   endpoint.Path,
				}
			}
		}
	}
	return nil
}

// Helper to get sorted endpoint keys
func (r *Reporter) getSortedEndpointKeys() []string {
	var keys []string
	for key := range r.stats.Endpoints {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
