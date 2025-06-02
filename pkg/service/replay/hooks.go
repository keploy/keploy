// Package replay provides the hooks for the replay service
package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Hooks struct {
	logger          *zap.Logger
	cfg             *config.Config
	tsConfigDB      TestSetConfig
	storage         Storage
	auth            service.Auth
	instrumentation Instrumentation
}

func NewHooks(logger *zap.Logger, cfg *config.Config, tsConfigDB TestSetConfig, storage Storage, auth service.Auth, instrumentation Instrumentation) TestHooks {
	return &Hooks{
		cfg:             cfg,
		logger:          logger,
		tsConfigDB:      tsConfigDB,
		storage:         storage,
		auth:            auth,
		instrumentation: instrumentation,
	}
}

func (h *Hooks) SimulateRequest(ctx context.Context, _ uint64, tc *models.TestCase, testSetID string) (interface{}, error) {
	switch tc.Kind {
	case models.HTTP:
		h.logger.Debug("Simulating HTTP request", zap.Any("Test case", tc))
		return pkg.SimulateHTTP(ctx, tc, testSetID, h.logger, h.cfg.Test.APITimeout)

	case models.GRPC_EXPORT:
		h.logger.Debug("Simulating gRPC request", zap.Any("Test case", tc))
		return pkg.SimulateGRPC(ctx, tc, testSetID, h.logger)

	default:
		return nil, fmt.Errorf("unsupported test case kind: %s", tc.Kind)
	}
}

func (h *Hooks) AfterTestSetRun(ctx context.Context, testSetID string, status bool) error {

	if h.cfg.Test.DisableMockUpload {
		return nil
	}

	if h.cfg.Test.BasePath != "" {
		h.logger.Debug("Mocking is disabled when basePath is given", zap.String("testSetID", testSetID), zap.String("basePath", h.cfg.Test.BasePath))
		return nil
	}

	if !status {
		return nil
	}

	// Inspect local mock file
	localMockPath := filepath.Join(h.cfg.Path, testSetID, "mocks.yaml")
	mockFileContent, err := os.ReadFile(localMockPath)
	if err != nil {
		h.logger.Error("Failed to read mock file for mock upload", zap.String("path", localMockPath), zap.Error(err))
		return nil
	}
	mockHash := utils.Hash(mockFileContent)
	mockFileReader := bytes.NewReader(mockFileContent)
	token, err := h.auth.GetToken(ctx)
	if err != nil || token == "" {
		h.logger.Error("Failed to Authenticate user, skipping mock upload", zap.Error(err))
		return nil
	}

	claims, err := extractClaimsWithoutVerification(token)
	var role, username string
	var ok bool
	if err != nil {
		h.logger.Error("Failed to extract claim from token for mock upload", zap.Error(err))
		return nil
	}

	if role, ok = claims["role"].(string); !ok || role == "" {
		h.logger.Error("Role not found in the token, skipping mock upload")
		return nil
	}

	if username, ok = claims["username"].(string); !ok {
		h.logger.Error("Username not found in the token, skipping mock upload")
		return nil
	}

	// get the plan of the current user

	plan, err := getLatestPlan(ctx, h.logger, h.cfg.APIServerURL, token)
	if err != nil {
		h.logger.Error("Failed to get latest plan of the user", zap.Error(err))
		return err
	}

	h.logger.Debug("The latest plan", zap.Any("Plan", plan))

	// Cross verify the local mock file with the test-set config
	tsConfig, err := h.tsConfigDB.Read(ctx, testSetID)
	// If test-set config is not found, upload the mock file
	if err != nil || tsConfig == nil || tsConfig.MockRegistry == nil {
		// create ts config
		var prescript, postscript string
		var template map[string]interface{}
		if tsConfig != nil {
			prescript = tsConfig.PreScript
			postscript = tsConfig.PostScript
			template = tsConfig.Template
		}
		tsConfig = &models.TestSet{
			PreScript:  prescript,
			PostScript: postscript,
			Template:   template,
			MockRegistry: &models.MockRegistry{
				Mock: mockHash,
				App:  h.cfg.AppName,
			},
		}

		if plan == "Free" {
			if username == "" {
				h.logger.Warn("Username not found in the token, skipping mock upload")
				return nil
			}
			tsConfig.MockRegistry.User = username
		}

		h.logger.Info("uploading mock file...")
		err = h.storage.Upload(ctx, mockFileReader, mockHash, h.cfg.AppName, token)
		if err != nil {
			h.logger.Error("Failed to upload mock file", zap.Error(err))
			return err
		}

		err := h.tsConfigDB.Write(ctx, testSetID, tsConfig)
		if err != nil {
			h.logger.Error("Failed to write test set config", zap.Error(err))
			return err
		}
		return nil
	}

	// If mock file is already uploaded, skip the upload
	if tsConfig.MockRegistry.Mock == mockHash {
		h.logger.Debug("Mock file is already uploaded, skipping upload", zap.String("testSetID", testSetID), zap.String("mockPath", localMockPath))
		return nil
	}

	// If mock file is changed, upload the new mock file
	h.logger.Debug("Mock file is changed, uploading new mock", zap.String("testSetID", testSetID), zap.String("mockPath", localMockPath))
	if token == "" {
		h.logger.Warn("Looks like you haven't logged in, skipping mock upload")
		h.logger.Warn("Please login using `keploy login` to upload the mock file")
		return nil
	}
	h.logger.Info("uploading mock file...")
	err = h.storage.Upload(ctx, mockFileReader, mockHash, h.cfg.AppName, token)
	if err != nil {
		h.logger.Error("Failed to upload mock file", zap.Error(err))
		return err
	}

	err = utils.AddToGitIgnore(h.logger, h.cfg.Path, "/*/mocks.yaml")
	if err != nil {
		utils.LogError(h.logger, err, "failed to add /*/mocks.yaml to .gitignore file")
	}

	return nil
}

func (h *Hooks) BeforeTestSetRun(ctx context.Context, testSetID string) error {

	if h.cfg.Test.BasePath != "" {
		h.logger.Debug("Mocking is disabled when basePath is given", zap.String("testSetID", testSetID), zap.String("basePath", h.cfg.Test.BasePath))
		return nil
	}

	if h.cfg.Test.UseLocalMock {
		h.logger.Debug("Using local mock file, as UseLocalMock is selected", zap.String("testSetID", testSetID))
		return nil
	}

	// Check if test-set config is present
	tsConfig, err := h.tsConfigDB.Read(ctx, testSetID)
	if err != nil || tsConfig == nil || tsConfig.MockRegistry == nil {
		h.logger.Debug("test set config for upload mock not found, continuing with local mock", zap.String("testSetID", testSetID), zap.Error(err))
		return nil
	}

	if tsConfig.MockRegistry.Mock == "" {
		h.logger.Warn("Mock is empty in test-set config, continuing with local mock if present", zap.String("testSetID", testSetID))
		return nil
	}

	if tsConfig.MockRegistry.App == "" {
		h.logger.Warn("App name is empty in test-set config, continuing with local mock if present", zap.String("testSetID", testSetID))
		return nil
	}

	// Check if mock file is already downloaded by previous test runs
	localMockPath := filepath.Join(h.cfg.Path, testSetID, "mocks.yaml")
	mockContent, err := os.ReadFile(localMockPath)
	if err == nil {
		if tsConfig.MockRegistry.Mock == utils.Hash(mockContent) {
			h.logger.Debug("Mock file already exists, downloading from cloud is not necessary", zap.String("testSetID", testSetID), zap.String("mockPath", localMockPath))
			return nil
		}
	}

	if tsConfig.MockRegistry.App != h.cfg.AppName {
		h.logger.Warn("App name in the keploy.yml does not match with the app name in the config.yml in the test-set", zap.String("test-set-config-AppName", tsConfig.MockRegistry.App), zap.String("global-config-Appname", h.cfg.AppName))
		h.logger.Warn("Using app name from the test-set's config.yml for mock retrieval", zap.String("appName", tsConfig.MockRegistry.App))
	}

	token, _ := h.auth.GetToken(ctx)

	h.logger.Info("Downloading mock file from cloud...", zap.String("testSetID", testSetID))
	cloudFile, err := h.storage.Download(ctx, tsConfig.MockRegistry.Mock, tsConfig.MockRegistry.App, tsConfig.MockRegistry.User, token)
	if err != nil {
		h.logger.Error("Failed to download mock file", zap.Error(err))
		return err
	}

	// Save the downloaded mock file to local
	file, err := os.Create(localMockPath)
	if err != nil {
		h.logger.Error("Failed to create local file", zap.String("path", localMockPath), zap.Error(err))
		return err
	}
	defer func() {
		err := file.Close()
		if err != nil {
			utils.LogError(h.logger, err, "failed to close the http response body")
		}
	}()

	done := make(chan struct{})

	// Spinner goroutine
	go func() {
		spinnerChars := []rune{'|', '/', '-', '\\'}
		i := 0
		for {
			select {
			case <-done:
				fmt.Print("\r") // Clear spinner line after done
				return
			default:
				fmt.Printf("\rDownloading... %c", spinnerChars[i%len(spinnerChars)])
				i++
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	_, err = io.Copy(file, cloudFile)
	if err != nil {
		close(done)
		return err
	}
	close(done)
	h.logger.Info("Mock file downloaded successfully")

	err = utils.AddToGitIgnore(h.logger, h.cfg.Path, "/*/mocks.yaml")
	if err != nil {
		utils.LogError(h.logger, err, "failed to add /*/mocks.yaml to .gitignore file")
	}

	return nil
}

func (h *Hooks) AfterTestRun(_ context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error {
	h.logger.Debug("AfterTestRun hook executed", zap.String("testRunID", testRunID), zap.Any("testSetIDs", testSetIDs), zap.Any("coverage", coverage))
	return nil
}

func (h *Hooks) GetConsumedMocks(ctx context.Context, id uint64) ([]models.MockState, error) {
	consumedMocks, err := h.instrumentation.GetConsumedMocks(ctx, id)
	if err != nil {
		h.logger.Error("failed to get consumed mocks", zap.Error(err))
		return nil, err
	}
	return consumedMocks, nil
}

// Function to parse and extract claims from a JWT token without verification
func extractClaimsWithoutVerification(tokenString string) (jwt.MapClaims, error) {
	token, _, err := new(jwt.Parser).ParseUnverified(tokenString, jwt.MapClaims{})
	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok {
		return claims, nil
	}
	return nil, fmt.Errorf("unable to parse claims")
}

type getPlanRes struct {
	Plan  models.Plan `json:"plan"`
	Error string      `json:"error"`
}

func getLatestPlan(ctx context.Context, logger *zap.Logger, serverUrl, token string) (string, error) {
	logger.Debug("Getting the latest plan", zap.String("serverUrl", serverUrl), zap.String("token", token))

	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/subscription/plan", serverUrl), nil)
	if err != nil {
		logger.Error("failed to create request", zap.Error(err))
		return "", fmt.Errorf("failed to get plan")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		logger.Error("http request failed", zap.Error(err))
		return "", fmt.Errorf("failed to get plan")
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			logger.Error("failed to close response body", zap.Error(cerr))
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("failed to read response body", zap.Error(err))
		return "", fmt.Errorf("failed to get plan")
	}

	var res getPlanRes
	if err := json.Unmarshal(body, &res); err != nil {
		logger.Error("failed to unmarshal response", zap.Error(err))
		return "", fmt.Errorf("failed to get plan")
	}

	if resp.StatusCode != http.StatusOK {
		logger.Error("non-200 response from subscription/plan", zap.Int("status", resp.StatusCode), zap.String("api_error", res.Error))
		if res.Error != "" {
			return "", fmt.Errorf("%s", res.Error)
		}
		return "", fmt.Errorf("failed to get plan")
	}

	if res.Plan.Type == "" {
		logger.Error("plan type missing in successful response", zap.Any("plan", res.Plan))
		return "", fmt.Errorf("plan not found")
	}

	return res.Plan.Type, nil
}
