package mockrecord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

	resolvedPath, err := resolveMockSetPath(basePath)
	if err != nil {
		return nil, err
	}
	basePath = resolvedPath

	sessionID := fmt.Sprintf("mock-%d", time.Now().Unix())
	mockFilePath := filepath.Join(basePath, "mocks.yaml")

	db := r.mockDB
	if db == nil || opts.Path != "" {
		db = mockdb.New(r.logger, basePath, "")
	}

	collector := newMetadataCollector()

	recordTimer := opts.Duration
	useTimer := opts.Duration > 0
	if !useTimer && r.cfg != nil && r.cfg.Record.RecordTimer > 0 {
		recordTimer = r.cfg.Record.RecordTimer
		useTimer = true
	}

	enableIncomingProxy := runtime.GOOS == "windows"
	result, err := r.runner.StartWithOptions(ctx, models.ReRecordCfg{}, record.StartOptions{
		Command:               opts.Command,
		TestSetID:             sessionID,
		RecordTimer:           recordTimer,
		UseRecordTimer:        useTimer,
		ProxyPort:             opts.ProxyPort,
		DNSPort:               opts.DNSPort,
		CaptureIncoming:       false,
		EnableIncomingProxy:   enableIncomingProxy,
		CaptureOutgoing:       true,
		RootMocksUntilSession: true,
		WriteTestSetConfig:    false,
		IgnoreAppError:        true,
		MockDB:                db,
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

func resolveMockSetPath(basePath string) (string, error) {
	basePath = filepath.Clean(strings.TrimSpace(basePath))
	if basePath == "" {
		basePath = "./keploy"
	}

	// If user explicitly passes a specific mock-set directory, use it as-is.
	if _, ok := parseMockSetID(filepath.Base(basePath)); ok {
		return basePath, nil
	}

	nextSetName, err := getNextMockSetName(basePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(basePath, nextSetName), nil
}

func getNextMockSetName(basePath string) (string, error) {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "mock-set-0", nil
		}
		return "", fmt.Errorf("failed to read mock base path %q: %w", basePath, err)
	}

	maxSetID := -1
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if setID, ok := parseMockSetID(entry.Name()); ok && setID > maxSetID {
			maxSetID = setID
		}
	}

	return fmt.Sprintf("mock-set-%d", maxSetID+1), nil
}

func parseMockSetID(name string) (int, bool) {
	const prefix = "mock-set-"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}

	rawID := strings.TrimPrefix(name, prefix)
	if rawID == "" {
		return 0, false
	}

	setID, err := strconv.Atoi(rawID)
	if err != nil || setID < 0 {
		return 0, false
	}
	return setID, true
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
