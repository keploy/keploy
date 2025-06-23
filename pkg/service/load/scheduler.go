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
}

func NewScheduler(logger *zap.Logger, config *config.Config, loadOptions *testsuite.LoadOptions, ts *testsuite.TestSuite, collector *MetricsCollector) *Scheduler {
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
	}
}

func (s *Scheduler) Run(parent context.Context) error {
	duration, err := time.ParseDuration(s.loadOptions.Duration)
	if err != nil {
		s.logger.Error("Failed to parse duration", zap.String("duration", s.loadOptions.Duration), zap.Error(err))
		return err
	}
	ctx, cancel := context.WithTimeout(parent, duration)
	s.cancelAll = cancel
	defer cancel()

	switch s.loadOptions.Profile {
	case "constant_vus":
		return s.runConstant(ctx, s.ts)
	case "ramping_vus":
		return s.runRamping(ctx, s.ts)
	default:
		return fmt.Errorf("unknown load profile %q", s.loadOptions.Profile)
	}
}

func (s *Scheduler) runConstant(ctx context.Context, ts *testsuite.TestSuite) error {
	err := s.spawnVUGoroutines(ctx, ts, s.loadOptions.VUs)
	if err != nil {
		s.logger.Error("Failed to spawn VU goroutines", zap.Int("vus", s.loadOptions.VUs), zap.Error(err))
		return err
	}

	<-ctx.Done()
	s.wg.Wait()

	return nil
}

func (s *Scheduler) runRamping(ctx context.Context, ts *testsuite.TestSuite) error {
	start := time.Now()
	current := 0
	for _, stg := range s.loadOptions.Stages {
		target := stg.Target
		delta := target - current
		if delta > 0 {
			if err := s.spawnVUGoroutines(ctx, ts, delta); err != nil {
				return err
			}
		}
		stageDuration, err := time.ParseDuration(stg.Duration)
		if err != nil {
			return err
		}
		sleepUntil := start.Add(stageDuration)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(sleepUntil)):
		}
		current = target
	}

	<-ctx.Done()
	s.wg.Wait()

	return nil
}

func (s *Scheduler) spawnVUGoroutines(ctx context.Context, ts *testsuite.TestSuite, n int) error {
	for i := 0; i < n; i++ {
		s.wg.Add(1)
		go func(id int) {
			vuWorker := NewVUWorker(s.config, s.logger, id+1, ts, s.collector, s.limiter, &s.wg)
			vuWorker.vuWorker(ctx)
		}(i)
	}
	return nil
}
