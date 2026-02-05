package mockrecord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	mockdb "go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type recorder struct {
	logger *zap.Logger
	cfg    *config.Config
	runner RecordRunner
	mockDB record.MockDB
}

// New creates a new mock recording service.
func New(logger *zap.Logger, cfg *config.Config, runner RecordRunner, mockDB record.MockDB) Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &recorder{
		logger: logger,
		cfg:    cfg,
		runner: runner,
		mockDB: mockDB,
	}
}

// Record captures outgoing calls while running the provided command.
func (r *recorder) Record(ctx context.Context, opts models.RecordOptions) (*models.RecordResult, error) {
	if r.runner == nil {
		return nil, errors.New("record service is not configured")
	}

	if strings.TrimSpace(opts.Command) == "" {
		return nil, errors.New("command is required")
	}

	basePath := strings.TrimSpace(opts.Path)
	if basePath == "" {
		if r.cfg != nil && r.cfg.Path != "" {
			basePath = r.cfg.Path
		} else {
			basePath = "./keploy"
		}
	}

	runID := fmt.Sprintf("run-%d", time.Now().Unix())
	if !strings.HasPrefix(filepath.Base(basePath), "run-") {
		basePath = filepath.Join(basePath, runID)
	}

	sessionID := fmt.Sprintf("mock-%d", time.Now().Unix())
	mockFilePath := filepath.Join(basePath, sessionID, "mocks.yaml")

	db := r.mockDB
	if db == nil || opts.Path != "" {
		db = mockdb.New(r.logger, basePath, "")
	}

	collector := newMetadataCollector(opts.Command)

	recordTimer := opts.Duration
	useTimer := opts.Duration > 0
	if !useTimer && r.cfg != nil && r.cfg.Record.RecordTimer > 0 {
		recordTimer = r.cfg.Record.RecordTimer
		useTimer = true
	}

	enableIncomingProxy := runtime.GOOS == "windows"
	result, err := r.runner.StartWithOptions(ctx, models.ReRecordCfg{}, record.StartOptions{
		Command:             opts.Command,
		TestSetID:           sessionID,
		RecordTimer:         recordTimer,
		UseRecordTimer:      useTimer,
		ProxyPort:           opts.ProxyPort,
		DNSPort:             opts.DNSPort,
		CaptureIncoming:     false,
		EnableIncomingProxy: enableIncomingProxy,
		CaptureOutgoing:     true,
		WriteTestSetConfig:  false,
		IgnoreAppError:      true,
		MockDB:              db,
		OnMock: func(mock *models.Mock) error {
			collector.addMock(mock)
			return nil
		},
	})
	if err != nil {
		return nil, err
	}

	mockCount := 0
	appExitCode := 0
	if result != nil {
		mockCount = result.MockCount
		appExitCode = exitCodeFromAppError(result.AppError)
	}

	if mockCount == 0 {
		if err := ensureMockFile(mockFilePath); err != nil {
			return nil, err
		}
	}

	return &models.RecordResult{
		MockFilePath: mockFilePath,
		Metadata:     collector.meta,
		MockCount:    mockCount,
		AppExitCode:  appExitCode,
		Output:       "",
	}, nil
}

func exitCodeFromAppError(appErr models.AppError) int {
	if appErr.Err == nil {
		return 0
	}

	if exitErr, ok := appErr.Err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 1
}

func ensureMockFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(utils.GetVersionAsComment()), 0o644)
}
