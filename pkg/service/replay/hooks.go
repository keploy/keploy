package replay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Hook struct {
	logger     *zap.Logger
	cfg        *config.Config
	tsConfigDB Config
	storage    Storage
	auth       Auth
}

func NewHook(logger *zap.Logger, cfg *config.Config, tsConfigDB Config, storage Storage, auth Auth) TestHooks {
	return &Hook{
		cfg:        cfg,
		logger:     logger,
		tsConfigDB: tsConfigDB,
		storage:    storage,
		auth:       auth,
	}
}

func (h *Hook) SimulateRequest(ctx context.Context, _ uint64, tc *models.TestCase, testSetID string) (*models.HTTPResp, error) {
	switch tc.Kind {
	case models.HTTP:
		h.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		resp, err := pkg.SimulateHTTP(ctx, *tc, testSetID, h.logger, h.cfg.Test.APITimeout)
		h.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
		return resp, err
	}
	return nil, nil
}

func (h *Hook) AfterTestSetRun(_ context.Context, testRunID, testSetID string, coverage models.TestCoverage, tsCnt int, status bool) (*models.TestReport, error) {
	h.logger.Debug("AfterTestHook", zap.Any("testRunID", testRunID), zap.Any("testSetID", testSetID), zap.Any("totalTestSetCount", tsCnt), zap.Any("coverage", coverage))
	return nil, nil
}

func (h *Hook) ProcessTestRunStatus(_ context.Context, status bool, testSetID string) {
	if status {
		h.logger.Debug("Test case passed for", zap.String("testSetID", testSetID))
	} else {
		h.logger.Debug("Test case failed for", zap.String("testSetID", testSetID))
	}
}

func (h *Hook) BeforeTestSetRun(ctx context.Context, testSetID string) error {

	if h.cfg.Test.BasePath != "" {
		h.logger.Debug("Mocking is disabled when basePath is given", zap.String("testSetID", testSetID), zap.String("basePath", h.cfg.Test.BasePath))
		return nil
	}

	if h.cfg.Test.DisableMockUpload {
		return nil
	}

	if h.cfg.Test.UseLocalMock {
		h.logger.Debug("Using local mock file, as UseLocalMock is selected", zap.String("testSetID", testSetID))
		return nil
	}

	testSetconfig, err := h.tsConfigDB.Read(ctx, testSetID)
	if err != nil || testSetconfig == nil || testSetconfig.MockRegistry == nil {
		h.logger.Debug("test set config for upload mock not found, continuing with local mock", zap.String("testSetID", testSetID), zap.Error(err))
		return nil
	}

	if testSetconfig.MockRegistry.Mock == "" {
		h.logger.Warn("Mock is empty in test-set config, continuing with local mock", zap.String("testSetID", testSetID))
		return nil
	}

	if testSetconfig.MockRegistry.App == "" {
		h.logger.Warn("App name is empty in test-set config, continuing with local mock", zap.String("testSetID", testSetID))
		return nil
	}

	// Check if mock file is already downloaded by previous test runs
	localMockPath := filepath.Join(h.cfg.Path, testSetID, "mocks.yaml")
	mockContent, err := os.ReadFile(localMockPath)
	if err == nil {
		if testSetconfig.MockRegistry.Mock == utils.Hash(mockContent) {
			h.logger.Debug("Mock file already exists, downloading from cloud is not necessary", zap.String("testSetID", testSetID), zap.String("mockPath", LocalMockPath))
			return nil
		}
	}

	// TODO autogenerate app name if not provided
	if testSetconfig.MockRegistry.App != h.cfg.AppName {
		h.logger.Warn("App name in the keploy.yml does not match with the app name in the config.yml in the test-set", zap.String("test-set-config-AppName", testSetconfig.MockRegistry.App), zap.String("global-config-Appname", h.cfg.AppName))
		h.logger.Warn("Using app name from the test-set's config.yml for mock retrival", zap.String("appName", testSetconfig.MockRegistry.App))
	}

	token := h.auth.GetToken(ctx)
	h.storage.Download(ctx, testSetconfig.MockRegistry.Mock, testSetconfig.MockRegistry.App, testSetconfig.MockRegistry.User, h.cfg.JWTToken)

	file, err := os.Create(localMockPath)
	if err != nil {
		h.logger.Error("Failed to create local file", zap.String("path", localMockPath), zap.Error(err))
		return err
	}
	defer file.Close()

	// TODO use platform package
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return err
	}

}
