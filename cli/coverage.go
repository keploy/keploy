package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"go.keploy.io/server/v3/pkg/coverage"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
	"github.com/spf13/cobra"
)

// Coverage returns the coverage command for the CLI
func Coverage(ctx context.Context, logger *zap.Logger, _ *config.Config, _ ServiceFactory, _ CmdConfigurator) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Generate mock replay coverage reports",
		Long: `Generate coverage reports from mock replays to see which mocks were used during test execution.

Supports multiple output formats:
  - JSON: keploy-coverage.json
  - Text: console summary with endpoint breakdown
  - HTML: interactive HTML report (optional)

Example:
  keploy coverage report
  keploy coverage report --html
  keploy coverage report --output ./reports/coverage.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return nil
		},
	}

	// Set up the report subcommand
	setupReportCmd()
	cmd.AddCommand(reportCmd)

	return cmd
}

var (
	htmlFlag      bool
	outputFlag    string
	runIDFlag     string
	testSetIDFlag string
)

func init() {
	Register("coverage", Coverage)
}

// reportCmd generates coverage reports
var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "Generate coverage report from latest test run",
	Long:  `Generates a coverage report from the latest test run, showing which mocks were used and which were missed.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return handleCoverageReport(cmd)
	},
}

func setupReportCmd() {
	reportCmd.Flags().BoolVar(&htmlFlag, "html", false, "Generate HTML report in addition to JSON and text")
	reportCmd.Flags().StringVar(&outputFlag, "output", "", "Custom output directory for coverage files (default: current directory)")
	reportCmd.Flags().StringVar(&runIDFlag, "run-id", "", "Test run id to include in the report (optional)")
	reportCmd.Flags().StringVar(&testSetIDFlag, "testset", "", "Filter report to a specific test set id (optional)")
}

// handleCoverageReport generates coverage reports from the global aggregator.
// It creates JSON, text, and optionally HTML reports from mock usage data collected during replay.
// Reports are written to the specified output directory.
// The global aggregator should be populated by the replay engine during test execution.
func handleCoverageReport(cmd *cobra.Command) error {
	stats := coverage.Global.Compute(runIDFlag, testSetIDFlag)

	// Check if any mocks were tracked
	if stats.TotalMocks == 0 {
		return fmt.Errorf("no mocks were tracked during replay; ensure test run completed and mocks were registered")
	}

	reporter := coverage.NewReporter(stats)

	// Determine output directory
	outputDir := "."
	if outputFlag != "" {
		outputDir = outputFlag
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory %q: %w", outputDir, err)
		}
	}

	// Generate JSON report
	jsonPath := filepath.Join(outputDir, "keploy-coverage.json")
	jsonContent, err := reporter.ToJSON()
	if err != nil {
		return fmt.Errorf("failed to generate JSON report: %w", err)
	}
	if err := os.WriteFile(jsonPath, []byte(jsonContent), 0644); err != nil {
		return fmt.Errorf("failed to write JSON report to %q: %w", jsonPath, err)
	}
	fmt.Printf("✅ JSON report written to: %s\n", jsonPath)

	// Generate text summary
	textPath := filepath.Join(outputDir, "keploy-coverage.txt")
	textContent := reporter.ToText()
	if err := os.WriteFile(textPath, []byte(textContent), 0644); err != nil {
		return fmt.Errorf("failed to write text report to %q: %w", textPath, err)
	}
	fmt.Printf("✅ Text report written to: %s\n", textPath)

	// Display text summary to console
	fmt.Println("\n" + textContent)

	// Generate HTML report if requested
	if htmlFlag {
		htmlPath := filepath.Join(outputDir, "keploy-coverage.html")
		htmlContent := reporter.ToHTML()
		if err := os.WriteFile(htmlPath, []byte(htmlContent), 0644); err != nil {
			return fmt.Errorf("failed to write HTML report to %q: %w", htmlPath, err)
		}
		fmt.Printf("✅ HTML report written to: %s\n", htmlPath)
	}

	return nil
}


