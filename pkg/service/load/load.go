package load

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/testsuite"
	"go.uber.org/zap"
)

type LTReport struct {
	TestSuiteFile string                `json:"test_suite_file"`
	VUs           int                   `json:"vus"`
	Duration      string                `json:"duration"`
	RPS           int                   `json:"rps"`
	Steps         []StepThresholdReport `json:"steps"`
}

type LoadTester struct {
	config    *config.Config
	logger    *zap.Logger
	testsuite *testsuite.TestSuite
	tsPath    string
	tsFile    string
	out       string
	insecure  bool
	profile   string
	vus       int
	duration  string
	rps       int
}

func NewLoadTester(cfg *config.Config, logger *zap.Logger) (*LoadTester, error) {
	testsuitePath := filepath.Join(cfg.Load.TSPath, cfg.Load.TSFile)
	logger.Info("Parsing TestSuite File", zap.String("path", testsuitePath))

	testsuite, err := testsuite.TSParser(testsuitePath)
	if err != nil {
		logger.Error("Failed to parse TestSuite file", zap.Error(err))
		return nil, fmt.Errorf("failed to parse TestSuite file: %w", err)
	}

	if testsuite.Spec.Load.Profile == "" {
		testsuite.Spec.Load.Profile = "constant_vus"
		logger.Info("Load profile not specified, defaulting to 'constant_vus'")
	}

	return &LoadTester{
		config:    cfg,
		logger:    logger,
		testsuite: &testsuite,
		tsPath:    cfg.Load.TSPath,
		tsFile:    cfg.Load.TSFile,
		out:       cfg.Load.Output,
		insecure:  cfg.Load.Insecure,
		profile:   testsuite.Spec.Load.Profile,
		vus:       testsuite.Spec.Load.VUs,
		duration:  testsuite.Spec.Load.Duration,
		rps:       testsuite.Spec.Load.RPS,
	}, nil
}

func (lt *LoadTester) Start(ctx context.Context) error {
	if lt.tsFile == "" {
		lt.logger.Error("Load test file is not specified")
		return fmt.Errorf("load test file is not specified, please provide a valid testsuite file using --file or -f flag")
	}

	if ctx.Value("vus") != nil && ctx.Value("vus") != 1 && lt.profile == "constant_vus" {
		lt.vus = ctx.Value("vus").(int)
		lt.logger.Debug("Overriding VUs from CLI", zap.Int("vus", lt.vus))
	}
	if ctx.Value("duration") != nil && ctx.Value("duration") != "" && lt.profile == "constant_vus" {
		lt.duration = ctx.Value("duration").(string)
		lt.logger.Debug("Overriding duration from CLI", zap.String("duration", lt.duration))
	}
	if ctx.Value("rps") != nil && ctx.Value("rps") != 0 && lt.profile == "constant_vus" {
		lt.rps = ctx.Value("rps").(int)
		lt.logger.Debug("Overriding RPS from CLI", zap.Int("rps", lt.rps))
	}

	loadOptions := &testsuite.LoadOptions{
		Profile:    lt.profile,
		VUs:        lt.vus,
		Duration:   lt.duration,
		RPS:        lt.rps,
		Stages:     lt.testsuite.Spec.Load.Stages,
		Thresholds: lt.testsuite.Spec.Load.Thresholds,
	}

	lt.logger.Info("Starting load test",
		zap.String("tsPath", lt.tsPath),
		zap.String("tsFile", lt.tsFile),
		zap.String("output", lt.out),
		zap.Int("vus", lt.vus),
		zap.String("duration", lt.duration),
		zap.Int("rps", lt.rps),
		zap.Bool("insecure", lt.insecure),
	)

	mc := NewMetricsCollector(lt.config, lt.logger, lt.vus)
	scheduler := NewScheduler(lt.logger, lt.config, loadOptions, lt.testsuite, mc)

	if err := scheduler.Run(ctx); err != nil {
		lt.logger.Error("Failed to run load test", zap.Error(err))
		return fmt.Errorf("failed to run load test: %w", err)
	}

	steps := mc.SetStepsMetrics()
	te := NewThresholdEvaluator(lt.config, lt.logger, lt.testsuite)
	report := te.Evaluate(steps)

	lt.printCLISummary(report)

	ltReport := LTReport{
		TestSuiteFile: lt.tsFile,
		VUs:           lt.vus,
		Duration:      lt.duration,
		RPS:           lt.rps,
		Steps:         report,
	}

	if lt.out == "json" {
		err := lt.saveJSONReport(ltReport)
		if err != nil {
			lt.logger.Error("Failed to save JSON report", zap.Error(err))
		}
	}

	lt.logger.Info("Load test completed", zap.String("tsFile", lt.tsFile))
	return nil
}

func (lt *LoadTester) printCLISummary(report []StepThresholdReport) {
	lt.logger.Info("Load test summary",
		zap.String("tsFile", lt.tsFile),
		zap.Int("vus", lt.vus),
		zap.String("duration", lt.duration),
		zap.Int("rps", lt.rps),
	)

	// Total Requests:      3,000
	// Failures:            18 (0.6%)
	// P95 Latency:         460ms
	// Data Sent:           1.2 MB
	// Data Received:       5.4 MB

	// Thresholds:
	//   ✓ http_req_duration_p95 < 500ms
	//   ✗ http_req_failed_rate <= 1%
	//   ✓ data_received > 1MB

	// Test Result: ❌ FAILED (1 critical threshold breached)

	for _, stepReport := range report {
		thresholdStatus := make(map[string]int)
		testResultStatus := "PASSED"

		fmt.Println("Step:", stepReport.StepName)
		fmt.Printf("  Total Requests:      %d\n", stepReport.TotalRequests)
		fmt.Printf("  Failures:            %d (%.2f%%)\n", stepReport.TotalFailures,
			float64(stepReport.TotalFailures)/float64(stepReport.TotalRequests)*100)
		fmt.Printf("  P95 Latency:         %s\n", stepReport.P95Latency)
		fmt.Printf("  Data Sent:           %.2f MB\n", float64(stepReport.TotalBytesOut)/(1024*1024))
		fmt.Printf("  Data Received:       %.2f MB\n", float64(stepReport.TotalBytesIn)/(1024*1024))
		fmt.Println("  Thresholds:")

		for _, th := range stepReport.Thresholds {
			status := "✓"
			if !th.Pass {
				status = "✗"
				thresholdStatus[th.Severity]++
				testResultStatus = "FAILED"
			}
			fmt.Printf("    %s %-25s %-25s \tActual(%v)\n", status, th.Metric, fmt.Sprintf("condition(%s)", th.Condition), th.Actual)
		}

		if testResultStatus == "FAILED" {
			fmt.Printf("  Test Result: ❌ %s ", testResultStatus)
			if thresholdStatus["critical"] > 0 {
				fmt.Printf("(%d critical threshold breached) ", thresholdStatus["critical"])
			}
			if thresholdStatus["high"] > 0 {
				fmt.Printf("(%d high threshold breached) ", thresholdStatus["high"])
			}
			if thresholdStatus["medium"] > 0 {
				fmt.Printf("(%d medium threshold breached) ", thresholdStatus["medium"])
			}
			if thresholdStatus["low"] > 0 {
				fmt.Printf("(%d low threshold breached) ", thresholdStatus["low"])
			}
			fmt.Printf("\n")
		} else {
			fmt.Printf("  Test Result: ✅ %s\n", testResultStatus)
		}
		fmt.Println(strings.Repeat("-", 100))
	}
}

func (lt *LoadTester) saveJSONReport(report LTReport) error {
	err := os.MkdirAll(filepath.Join("keploy", "load", "reports"), 0755)
	if err != nil {
		lt.logger.Error("Failed to create reports directory", zap.Error(err))
		return fmt.Errorf("failed to create reports directory: %w", err)
	}
	filePath := filepath.Join("keploy", "load", "reports", fmt.Sprintf("%s.json", strings.TrimSuffix(lt.tsFile, filepath.Ext(lt.tsFile))))
	file, err := os.Create(filePath)
	if err != nil {
		lt.logger.Error("Failed to create output file", zap.Error(err))
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		lt.logger.Error("Failed to encode report to JSON", zap.Error(err))
		return fmt.Errorf("failed to encode report to JSON: %w", err)
	}

	lt.logger.Info("Report saved successfully", zap.String("output", filePath))
	return nil
}
