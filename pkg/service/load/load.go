package load

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/testsuite"
	"go.uber.org/zap"
)

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

/*
LoadOptionsDefaults defines the default options for load testing.
It can be used to check if enterd values from CLI are default or not.
if defaults then do nothing, if not then use the values from CLI.
*/
// type LoadOptionsDefaults struct {
// 	VUs      int
// 	Duration string
// 	RPS      int
// }

func NewLoadTester(cfg *config.Config, logger *zap.Logger) (*LoadTester, error) {
	testsuitePath := filepath.Join(cfg.Load.TSPath, cfg.Load.TSFile)
	logger.Info("Parsing TestSuite File", zap.String("path", testsuitePath))

	testsuite, err := testsuite.TSParser(testsuitePath)
	if err != nil {
		logger.Error("Failed to parse TestSuite file", zap.Error(err))
		return nil, fmt.Errorf("failed to parse TestSuite file: %w", err)
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

	if ctx.Value("vus") != nil && ctx.Value("vus") != 1 {
		lt.vus = ctx.Value("vus").(int)
		lt.logger.Debug("Overriding VUs from CLI", zap.Int("vus", lt.vus))
	}
	if ctx.Value("duration") != nil && ctx.Value("duration") != "" {
		lt.duration = ctx.Value("duration").(string)
		lt.logger.Debug("Overriding duration from CLI", zap.String("duration", lt.duration))
	}
	if ctx.Value("rps") != nil && ctx.Value("rps") != 0 {
		lt.rps = ctx.Value("rps").(int)
		lt.logger.Debug("Overriding RPS from CLI", zap.Int("rps", lt.rps))
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

	d, err := time.ParseDuration(lt.duration)
	if err != nil {
		lt.logger.Error("Failed to parse duration", zap.Error(err))
		return fmt.Errorf("failed to parse duration: %w", err)
	}

	// Create a context with timeout for the load test
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	var wg sync.WaitGroup

	for i := 0; i < lt.vus; i++ {
		wg.Add(1)
		vuID := i + 1
		lt.logger.Debug("Starting virtual user", zap.Int("vuID", vuID))
		go func(vuID int) {
			defer wg.Done()
			options := testsuite.LoadOptions{
				Profile:    lt.profile,
				VUs:        lt.vus,
				Duration:   lt.duration,
				RPS:        lt.rps,
				Stages:     lt.testsuite.Spec.Load.Stages,
				Thresholds: lt.testsuite.Spec.Load.Thresholds,
			}
			if err := lt.runVirtualUser(&ctx, options, vuID); err != nil {
				lt.logger.Error("Failed to run virtual user", zap.Int("vuID", vuID), zap.Error(err))
			}
		}(vuID)
	}

	wg.Wait()
	lt.logger.Info("Load test completed", zap.String("tsFile", lt.tsFile))
	return nil
}

func (lt *LoadTester) runVirtualUser(ctx *context.Context, options testsuite.LoadOptions, vuID int) error {
	lt.logger.Debug("Running virtual user", zap.Int("vuID", vuID))

	tsExec, err := testsuite.NewTSExecutor(lt.config, lt.logger, true)
	if err != nil {
		lt.logger.Error("Failed to create TestSuite executor", zap.Int("vuID", vuID), zap.Error(err))
		return fmt.Errorf("failed to create TestSuite executor: %w", err)
	}
	// NOTE (for future pop up questions): Use the passed (options) parameter istead of the (LoadOptions) of the testsuite
	// cause it got the overwritten by the CLI.
	tsExec.Testsuite = lt.testsuite

	if err := tsExec.Execute(ctx); err != nil {
		lt.logger.Error("Failed to execute TestSuite", zap.Int("vuID", vuID), zap.Error(err))
		return fmt.Errorf("failed to execute TestSuite: %w", err)
	}
	lt.logger.Info("Virtual user completed", zap.Int("vuID", vuID))
	return nil
}
