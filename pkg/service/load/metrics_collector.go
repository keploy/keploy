package load

import (
	"sync"
	"time"

	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

type MetricsCollector struct {
	config     *config.Config
	logger     *zap.Logger
	VUsReports []VUReport
	mu         sync.RWMutex
}

type StepMetrics struct {
	StepName         string
	StepCount        int
	StepFailure      int
	StepResponseTime []time.Duration
	StepBytesIn      int64
	StepBytesOut     int64
}

func NewMetricsCollector(config *config.Config, logger *zap.Logger, vus int) *MetricsCollector {
	return &MetricsCollector{
		config:     config,
		logger:     logger,
		VUsReports: make([]VUReport, vus),
	}
}

func (mc *MetricsCollector) SetStepsMetrics() []StepMetrics {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	if len(mc.VUsReports) == 0 || len(mc.VUsReports[0].Steps) == 0 {
		mc.logger.Warn("No VU reports or steps available for metrics calculation")
		return nil
	}

	steps := make([]StepMetrics, len(mc.VUsReports[0].Steps))
	for i, vuReport := range mc.VUsReports {
		for j, step := range vuReport.Steps {
			// Initialize per step metrics. step 1, 2, 3 and so on. it the same step across all VUs but with different results.
			if i == 0 {
				steps[j] = StepMetrics{
					StepName:         step.StepName,
					StepCount:        0,
					StepFailure:      0,
					StepResponseTime: make([]time.Duration, 0),
					StepBytesIn:      0,
					StepBytesOut:     0,
				}
			}
			// collecting the results from different VUs into one place to operate on later.
			steps[j].StepCount += step.StepCount
			steps[j].StepFailure += step.StepFailure
			steps[j].StepResponseTime = append(steps[j].StepResponseTime, step.StepResponseTime...)
			steps[j].StepBytesIn += step.StepBytesIn
			steps[j].StepBytesOut += step.StepBytesOut
		}
	}

	for _, step := range steps {
		mc.logger.Debug("Step Metrics",
			zap.String("stepName", step.StepName),
			zap.Int("stepCount", step.StepCount),
			zap.Int("stepFailure", step.StepFailure),
			zap.Any("stepResponseTime", step.StepResponseTime),
			zap.Int64("stepBytesIn", step.StepBytesIn),
			zap.Int64("stepBytesOut", step.StepBytesOut),
		)
	}

	return steps
}

func (mc *MetricsCollector) CollectVUReport(vuReport *VUReport) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.VUsReports[vuReport.VUID] = *vuReport
	mc.logger.Debug("VU Report collected",
		zap.Int("vuID", vuReport.VUID),
		zap.Int("tsExecCount", vuReport.TSExecCount),
		zap.Int("tsExecFailure", vuReport.TSExecFailure),
		zap.Any("tsExecTime", vuReport.TSExecTime),
		zap.Int("totalVUs", len(mc.VUsReports)),
	)
}
