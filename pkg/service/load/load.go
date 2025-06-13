package load

import (
	"context"
	"fmt"
	"path/filepath"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/testsuite"
	"go.uber.org/zap"
)

type LoadTester struct {
	config   *config.Config
	logger   *zap.Logger
	tsPath   string
	tsFile   string
	out      string
	vus      int
	duration string
	rps      int
	insecure bool
}

func NewLoadTester(cfg *config.Config, logger *zap.Logger) (*LoadTester, error) {
	return &LoadTester{
		config:   cfg,
		logger:   logger,
		tsPath:   cfg.Load.TSPath,
		tsFile:   cfg.Load.TSFile,
		out:      cfg.Load.Output,
		vus:      cfg.Load.VUs,
		duration: cfg.Load.Duration,
		rps:      cfg.Load.RPS,
		insecure: cfg.Load.Insecure,
	}, nil
}

func (lt *LoadTester) Start(ctx context.Context) error {
	lt.logger.Info("Starting load test",
		zap.String("tsPath", lt.tsPath),
		zap.String("tsFile", lt.tsFile),
		zap.String("output", lt.out),
		zap.Int("vus", lt.vus),
		zap.String("duration", lt.duration),
		zap.Int("rps", lt.rps),
		zap.Bool("insecure", lt.insecure),
	)
	if lt.tsFile == "" {
		lt.logger.Error("Load test file is not specified")
		return fmt.Errorf("load test file is not specified, please provide a valid testsuite file using --file or -f flag")
	}

	testsuitePath := filepath.Join(lt.tsPath, lt.tsFile)
	lt.logger.Info("Parsing TestSuite File", zap.String("path", testsuitePath))

	testsuite, err := testsuite.TSParser(testsuitePath)
	if err != nil {
		lt.logger.Error("Failed to parse TestSuite file", zap.Error(err))
		return fmt.Errorf("failed to parse TestSuite file: %w", err)
	}

	lt.logger.Info("TestSuite parsed successfully",
		zap.String("name", testsuite.Name),
		zap.String("version", testsuite.Version),
		zap.String("kind", testsuite.Kind),
		zap.String("description", testsuite.Spec.Metadata.Description),
	)

	lt.logger.Info("Load test configuration",
		zap.String("Profile", testsuite.Spec.Load.Profile),
		zap.Int("VUs", testsuite.Spec.Load.VUs),
		zap.String("Duration", testsuite.Spec.Load.Duration),
		zap.Int("RPS", testsuite.Spec.Load.RPS),
		zap.Any("Stages", testsuite.Spec.Load.Stages),
		zap.Any("Thresholds", testsuite.Spec.Load.Thresholds),
		zap.String("Output", lt.out),
		zap.Bool("Insecure", lt.insecure),
	)

	return nil
}
