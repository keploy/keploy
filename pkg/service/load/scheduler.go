package load

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/testsuite"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

type Scheduler struct {
	config      *config.Config
	logger      *zap.Logger
	loadOptions *testsuite.LoadOptions
	ts          *testsuite.TestSuite
	collector   *MetricsCollector
	limiter     *rate.Limiter
	cancelAll   context.CancelFunc
	wg          sync.WaitGroup
	vuCownter   int
}

func NewScheduler(logger *zap.Logger, config *config.Config, loadOptions *testsuite.LoadOptions, ts *testsuite.TestSuite, collector *MetricsCollector) *Scheduler {
	// setting the rate limiter based on the RPS specified in loadOptions will be passed later to the TSExecutor execute function.
	var lim *rate.Limiter
	if loadOptions.RPS > 0 {
		lim = rate.NewLimiter(rate.Limit(loadOptions.RPS), loadOptions.RPS)
	}

	return &Scheduler{
		loadOptions: loadOptions,
		ts:          ts,
		collector:   collector,
		limiter:     lim,
		logger:      logger,
		config:      config,
		vuCownter:   0,
	}
}

func (s *Scheduler) Run(parent context.Context, exporter *Exporter) error {
	// setting the context with a timeout based on the duration specified in loadOptions.
	// will be given to the VUWorker goroutines along with the waitgroup to synchronize.
	duration, err := time.ParseDuration(s.loadOptions.Duration)
	if err != nil {
		s.logger.Error("Failed to parse duration", zap.String("duration", s.loadOptions.Duration), zap.Error(err))
		return err
	}
	ctx, cancel := context.WithTimeout(parent, duration)
	s.cancelAll = cancel
	defer cancel()

	exporter.ltToken.CreatedAt = time.Now()

	// check if the loadOptions has a valid profile set, if not return an error.
	switch s.loadOptions.Profile {
	case "constant_vus":
		return s.runConstant(ctx, s.ts, exporter)
	case "ramping_vus":
		return s.runRamping(ctx, s.ts, exporter)
	default:
		return fmt.Errorf("unknown load profile %q", s.loadOptions.Profile)
	}
}

func (s *Scheduler) runConstant(ctx context.Context, ts *testsuite.TestSuite, exporter *Exporter) error {
	exporter.StartServer(ctx)
	exporter.ExportLoadTestToken()
	err := s.spawnVUGoroutines(ctx, ts, s.loadOptions.VUs, exporter)
	if err != nil {
		s.logger.Error("Failed to spawn VU goroutines", zap.Int("vus", s.loadOptions.VUs), zap.Error(err))
		return err
	}

	// if context is done it waits for all VU goroutines to finish reporting back to the MetricCollector.
	<-ctx.Done()
	s.wg.Wait()

	return nil
}

func (s *Scheduler) runRamping(ctx context.Context, ts *testsuite.TestSuite, exporter *Exporter) error {
	exporter.StartServer(ctx)
	exporter.ExportLoadTestToken()
	start := time.Now()
	current := 0
	cumulative := start
	for _, stg := range s.loadOptions.Stages {
		// spawning VU goroutines based on the target specified in the stage.
		target := stg.Target
		delta := target - current
		if delta > 0 {
			if err := s.spawnVUGoroutines(ctx, ts, delta, exporter); err != nil {
				return err
			}
		}

		stageDuration, err := time.ParseDuration(stg.Duration)
		if err != nil {
			return err
		}
		cumulative = cumulative.Add(stageDuration)
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return nil
		case <-time.After(time.Until(cumulative)):
		}
		current = target
	}

	// if context is done it waits for all VU goroutines to finish reporting back to the MetricCollector.

	<-ctx.Done()
	s.wg.Wait()

	return nil
}

func (s *Scheduler) spawnVUGoroutines(ctx context.Context, ts *testsuite.TestSuite, n int, exporter *Exporter) error {
	startID := s.vuCownter
	endID := s.vuCownter + n
	for id := startID; id < endID; id++ {
		s.wg.Add(1)
		// spawning VUWorker goroutines with the context, test suite, metrics collector, rate limiter and waitgroup.
		// the VUWorker will execute the test suite steps and report the results back to the MetricsCollector.
		go func(id int) {
			vuWorker := NewVUWorker(s.config, s.logger, id, ts, s.collector, s.limiter, &s.wg, exporter)
			vuWorker.vuWorker(ctx)
		}(id)
	}
	s.vuCownter += n
	return nil
}
