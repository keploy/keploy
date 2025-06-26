package load

import (
	"context"
	"sync"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/testsuite"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

type VUWorker struct {
	config  *config.Config
	logger  *zap.Logger
	VUID    int
	ts      *testsuite.TestSuite
	mc      *MetricsCollector
	limiter *rate.Limiter
	waitG   *sync.WaitGroup
}

type VUReport struct {
	VUID          int
	TSExecCount   int
	TSExecFailure int
	TSExecTime    []time.Duration
	Steps         []StepReport
}

type StepReport struct {
	StepName         string
	StepCount        int
	StepResponseTime []time.Duration
	StepResults      []testsuite.StepResult
}

func NewVUWorker(cfg *config.Config, logger *zap.Logger, id int, ts *testsuite.TestSuite, col *MetricsCollector, lim *rate.Limiter, wg *sync.WaitGroup) *VUWorker {
	return &VUWorker{
		config:  cfg,
		logger:  logger,
		VUID:    id,
		ts:      ts,
		mc:      col,
		limiter: lim,
		waitG:   wg,
	}
}

func (w *VUWorker) vuWorker(ctx context.Context) {
	VUReport := &VUReport{
		VUID:          w.VUID,
		TSExecCount:   0,
		TSExecFailure: 0,
		TSExecTime:    make([]time.Duration, 0),
		Steps:         make([]StepReport, 0),
	}
	w.logger.Debug("Running virtual user", zap.Int("vuID", w.VUID))

	for _, step := range w.ts.Spec.Steps {
		VUReport.Steps = append(VUReport.Steps, StepReport{
			StepName:         step.Name,
			StepCount:        0,
			StepResponseTime: make([]time.Duration, 0),
			StepResults:      make([]testsuite.StepResult, 0),
		})
	}

	tsExec, err := testsuite.NewTSExecutor(w.config, w.logger, true)
	if err != nil {
		w.logger.Error("Failed to create TestSuite executor", zap.Int("vuID", w.VUID), zap.Error(err))
	}

	// Set the TestSuite for the executor manually after skipping the parsing.
	tsExec.Testsuite = w.ts

	for {
		select {
		case <-ctx.Done():
			// if the context duration is done, waits for reporting to the MetricsCollector.
			w.mc.CollectVUReport(VUReport)
			w.logger.Debug("Virtual user context done", zap.Int("vuID", w.VUID))
			w.waitG.Done()
			return
		default:
			execReport, err := tsExec.Execute(ctx, w.limiter)
			if err != nil {
				w.logger.Error("Failed to execute TestSuite", zap.Int("vuID", w.VUID), zap.Error(err))
				VUReport.TSExecFailure++
				// TODO: stop or continue
				// return
			}
			w.logger.Debug("Virtual user executed TestSuite", zap.Int("vuID", w.VUID))
			VUReport.TSExecCount++
			VUReport.TSExecTime = append(VUReport.TSExecTime, execReport.ExecutionTime)

			// collecting per step results.
			for i, step := range execReport.StepsResult {
				VUReport.Steps[i].StepCount++
				VUReport.Steps[i].StepResponseTime = append(VUReport.Steps[i].StepResponseTime, step.ResponseTime)
				VUReport.Steps[i].StepResults = append(VUReport.Steps[i].StepResults, step)
			}
		}
	}
}
