package testsuite

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

type TSExecutor struct {
	config  *config.Config
	logger  *zap.Logger
	client  *http.Client
	baseURL string
	tsPath  string
}

func NewTSExecutor(cfg *config.Config, logger *zap.Logger) (*TSExecutor, error) {
	if cfg.TestSuite.TSPath == "" {
		cfg.TestSuite.TSPath = "keploy/testsuite"
		logger.Info("Using default test suite path", zap.String("path", cfg.TestSuite.TSPath))
	}

	return &TSExecutor{
		config: cfg,
		logger: logger,
		client: &http.Client{
			Timeout: time.Duration(30) * time.Second,
			Transport: &http.Transport{
				// disable tls check
				//nolint:gosec
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
		baseURL: cfg.TestSuite.BaseURL,
		tsPath:  cfg.TestSuite.TSPath,
	}, nil
}

func (e *TSExecutor) Execute(ctx context.Context) error {
	if e.baseURL == "" {
		e.logger.Error("base URL is not set for the test suite execution")
		return fmt.Errorf("base URL is not set for the test suite execution")
	}

	if e.tsPath == "" {
		e.logger.Error("test suite path is not set")
		return fmt.Errorf("test suite path is not set, use --ts-path flag to set it")
	}

	e.logger.Info("executing test suite", zap.String("path", e.tsPath), zap.String("baseURL", e.baseURL))

	// ---------------- expermental (not completed yet) -------------------------
	testsuitePath := e.tsPath + "/suite-0.yaml"
	e.logger.Debug("parsing test suite file", zap.String("file", testsuitePath))

	testsuite, err := TSParser(testsuitePath)
	if err != nil {
		e.logger.Error("failed to parse test suite", zap.Error(err))
		return err
	}

	fmt.Println(testsuite)

	return nil // Placeholder for actual implementation
}

// func (e *TSExecutor) executeSuite() (*SuiteResult, error) {
// 	return &SuiteResult{}, nil // Placeholder for actual implementation
// }
