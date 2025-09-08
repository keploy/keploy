package load

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/testsuite"
	"go.uber.org/zap"
)

type ThresholdReport struct {
	Metric    string      `json:"metric"`
	Condition string      `json:"condition"`
	Severity  string      `json:"severity"`
	Comment   string      `json:"comment,omitempty"`
	Actual    interface{} `json:"actual"`
	Pass      bool        `json:"pass"`
}

type StepThresholdReport struct {
	StepName      string            `json:"step_name"`
	TotalRequests int               `json:"total_requests"`
	TotalFailures int               `json:"total_failures"`
	TotalBytesIn  int64             `json:"total_bytes_in"`
	TotalBytesOut int64             `json:"total_bytes_out"`
	P95Latency    time.Duration     `json:"p95_latency"`
	Thresholds    []ThresholdReport `json:"thresholds"`
}

type ThresholdEvaluator struct {
	config *config.Config
	logger *zap.Logger
	ts     *testsuite.TestSuite
}

func NewThresholdEvaluator(cfg *config.Config, logger *zap.Logger, ts *testsuite.TestSuite) *ThresholdEvaluator {
	return &ThresholdEvaluator{
		config: cfg,
		logger: logger,
		ts:     ts,
	}
}

func (te *ThresholdEvaluator) Evaluate(steps []StepMetrics) []StepThresholdReport {
	thresholds := te.ts.Spec.Load.Thresholds
	if len(thresholds) == 0 {
		te.logger.Info("No thresholds defined in TestSuite, skipping evaluation")
		return nil
	}

	var reports []StepThresholdReport

	for _, step := range steps {
		var allResponseTimes = step.StepResponseTime
		totalRequests := step.StepCount
		totalFailures := step.StepFailure
		totalBytesIn := step.StepBytesIn
		totalBytesOut := step.StepBytesOut

		// Calculate P95 latency
		// getting the index and value of 95th percentile from the response times.
		var p95Idx int
		var p95 time.Duration
		if len(allResponseTimes) > 0 {
			sort.Slice(allResponseTimes, func(i, j int) bool { return allResponseTimes[i] < allResponseTimes[j] })
			idx := int(math.Ceil(float64(len(allResponseTimes))*0.95)) - 1
			if idx < 0 {
				idx = 0
			}
			p95Idx = idx
			p95 = allResponseTimes[p95Idx]
		}

		// Calculate failed rate as a percentage
		// If totalRequests is 0, we avoid division by zero by setting failedRate to 0.
		var failedRate float64
		if totalRequests > 0 {
			failedRate = (float64(totalFailures) / float64(totalRequests)) * 100
		}

		// Convert bytes to MB for reporting
		// 1 MB = 1024 * 1024 bytes
		dataReceivedMB := float64(totalBytesIn) / (1024 * 1024)
		dataSentMB := float64(totalBytesOut) / (1024 * 1024)

		var stepReport StepThresholdReport
		stepReport.StepName = step.StepName
		stepReport.TotalRequests = totalRequests
		stepReport.TotalFailures = totalFailures
		stepReport.TotalBytesIn = totalBytesIn
		stepReport.TotalBytesOut = totalBytesOut
		stepReport.P95Latency = p95
		stepReport.Thresholds = make([]ThresholdReport, 0, len(thresholds))

		for _, th := range thresholds {
			switch th.Metric {
			case "http_req_duration_p95":
				pass := compareDuration(p95, th.Condition)
				te.logger.Debug("Threshold check",
					zap.String("step", step.StepName),
					zap.String("metric", th.Metric),
					zap.String("condition", th.Condition),
					zap.String("actual", p95.String()),
					zap.Bool("pass", pass),
					zap.String("severity", th.Severity),
					zap.String("comment", th.Comment),
				)
				if !pass {
					te.logger.Debug(fmt.Sprintf("Threshold failed: %s %s (actual: %s) for step %s", th.Metric, th.Condition, p95, step.StepName))
				}
				stepReport.Thresholds = append(stepReport.Thresholds, ThresholdReport{
					Metric:    th.Metric,
					Condition: th.Condition,
					Actual:    p95.String(),
					Pass:      pass,
					Severity:  th.Severity,
					Comment:   th.Comment,
				})
			case "http_req_failed_rate":
				pass := compareFloat(failedRate, th.Condition)
				te.logger.Debug("Threshold check",
					zap.String("step", step.StepName),
					zap.String("metric", th.Metric),
					zap.String("condition", th.Condition),
					zap.Float64("actual", failedRate),
					zap.Bool("pass", pass),
					zap.String("severity", th.Severity),
					zap.String("comment", th.Comment),
				)
				if !pass {
					te.logger.Debug(fmt.Sprintf("Threshold failed: %s %s (actual: %.2f%%) for step %s", th.Metric, th.Condition, failedRate, step.StepName))
				}
				stepReport.Thresholds = append(stepReport.Thresholds, ThresholdReport{
					Metric:    th.Metric,
					Condition: th.Condition,
					Actual:    failedRate,
					Pass:      pass,
					Severity:  th.Severity,
					Comment:   th.Comment,
				})
			case "data_received":
				pass := compareFloat(dataReceivedMB, th.Condition)
				te.logger.Debug("Threshold check",
					zap.String("step", step.StepName),
					zap.String("metric", th.Metric),
					zap.String("condition", th.Condition),
					zap.Float64("actual", dataReceivedMB),
					zap.Bool("pass", pass),
					zap.String("severity", th.Severity),
					zap.String("comment", th.Comment),
				)
				if !pass {
					te.logger.Debug(fmt.Sprintf("Threshold failed: %s %s (actual: %.2f MB) for step %s", th.Metric, th.Condition, dataReceivedMB, step.StepName))
				}
				stepReport.Thresholds = append(stepReport.Thresholds, ThresholdReport{
					Metric:    th.Metric,
					Condition: th.Condition,
					Actual:    dataReceivedMB,
					Pass:      pass,
					Severity:  th.Severity,
					Comment:   th.Comment,
				})
			case "data_sent":
				pass := compareFloat(dataSentMB, th.Condition)
				te.logger.Debug("Threshold check",
					zap.String("step", step.StepName),
					zap.String("metric", th.Metric),
					zap.String("condition", th.Condition),
					zap.Float64("actual", dataSentMB),
					zap.Bool("pass", pass),
					zap.String("severity", th.Severity),
					zap.String("comment", th.Comment),
				)
				if !pass {
					te.logger.Debug(fmt.Sprintf("Threshold failed: %s %s (actual: %.2f MB) for step %s", th.Metric, th.Condition, dataSentMB, step.StepName))
				}
				stepReport.Thresholds = append(stepReport.Thresholds, ThresholdReport{
					Metric:    th.Metric,
					Condition: th.Condition,
					Actual:    dataSentMB,
					Pass:      pass,
					Severity:  th.Severity,
					Comment:   th.Comment,
				})
			default:
				te.logger.Warn("Unknown threshold metric", zap.String("metric", th.Metric))
			}
		}
		reports = append(reports, stepReport)
	}
	return reports
}

// compareDuration compares a time.Duration value with a condition string.
// The condition string can be in the format of "<", "<=", ">", ">=", "=",
// and can include a duration value like "500ms", "1s", etc.
// Returns true if the condition is met, false otherwise.
func compareDuration(val time.Duration, cond string) bool {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return true
	}
	// seperate the given string into operator and value
	// e.g. "<= 500ms" -> op = "<=", cmpStr = "500ms"
	var op string
	var cmpStr string
	if strings.HasPrefix(cond, "<=") {
		op = "<="
		cmpStr = strings.TrimSpace(cond[2:])
	} else if strings.HasPrefix(cond, "<") {
		op = "<"
		cmpStr = strings.TrimSpace(cond[1:])
	} else if strings.HasPrefix(cond, ">=") {
		op = ">="
		cmpStr = strings.TrimSpace(cond[2:])
	} else if strings.HasPrefix(cond, ">") {
		op = ">"
		cmpStr = strings.TrimSpace(cond[1:])
	} else if strings.HasPrefix(cond, "=") {
		op = "="
		cmpStr = strings.TrimSpace(cond[1:])
	} else {
		return false
	}
	cmpDur, err := time.ParseDuration(cmpStr)
	if err != nil {
		return false
	}
	switch op {
	case "<":
		return val < cmpDur
	case "<=":
		return val <= cmpDur
	case ">":
		return val > cmpDur
	case ">=":
		return val >= cmpDur
	case "=":
		return val == cmpDur
	}
	return false
}

// compareFloat compares a float64 value with a condition string.
// The condition string can be in the format of "<", "<=", ">", ">=", "=",
// and can include a value like "50%", "100MB", etc.
// Returns true if the condition is met, false otherwise.
func compareFloat(val float64, cond string) bool {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return true
	}
	// separate the given string into operator and value
	// e.g. "<= 50%" -> op = "<=", cmpStr = "50%"
	// e.g. "> 100MB" -> op = ">", cmpStr = "100MB"
	var op string
	var cmpStr string
	if strings.HasPrefix(cond, "<=") {
		op = "<="
		cmpStr = strings.TrimSpace(cond[2:])
	} else if strings.HasPrefix(cond, "<") {
		op = "<"
		cmpStr = strings.TrimSpace(cond[1:])
	} else if strings.HasPrefix(cond, ">=") {
		op = ">="
		cmpStr = strings.TrimSpace(cond[2:])
	} else if strings.HasPrefix(cond, ">") {
		op = ">"
		cmpStr = strings.TrimSpace(cond[1:])
	} else if strings.HasPrefix(cond, "=") {
		op = "="
		cmpStr = strings.TrimSpace(cond[1:])
	} else {
		return false
	}
	// Remove % or MB if present
	cmpStr = strings.TrimSuffix(cmpStr, "%")
	cmpStr = strings.TrimSuffix(cmpStr, "MB")
	cmpStr = strings.TrimSpace(cmpStr)
	cmpVal := 0.0
	fmt.Sscanf(cmpStr, "%f", &cmpVal)
	switch op {
	case "<":
		return val < cmpVal
	case "<=":
		return val <= cmpVal
	case ">":
		return val > cmpVal
	case ">=":
		return val >= cmpVal
	case "=":
		return val == cmpVal
	}
	return false
}
