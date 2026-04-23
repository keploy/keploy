// Package replay provides the hooks for the replay service
package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type Hooks struct {
	logger          *zap.Logger
	cfg             *config.Config
	tsConfigDB      TestSetConfig
	storage         Storage
	auth            service.Auth
	instrumentation Instrumentation
	mock            *mock
}

func NewHooks(logger *zap.Logger, cfg *config.Config, tsConfigDB TestSetConfig, storage Storage, auth service.Auth, instrumentation Instrumentation, mock *mock) TestHooks {
	return &Hooks{
		cfg:             cfg,
		logger:          logger,
		tsConfigDB:      tsConfigDB,
		storage:         storage,
		auth:            auth,
		instrumentation: instrumentation,
		mock:            mock,
	}
}

func (h *Hooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {

	// Extract URL replacements and port mappings: merge global + per-test-set
	// (test-set level overrides global for same key)
	urlReplacements, portMappings := h.mergeReplaceWith(testSetID)

	switch tc.Kind {
	case models.HTTP:
		if err := h.instrumentation.BeforeSimulate(ctx, &tc.HTTPReq.Timestamp, testSetID, tc.Name); err != nil {
			h.logger.Error("failed to call BeforeSimulate hook", zap.Error(err))
		}

		hostToUse := h.cfg.Test.Host
		if hostToUse == "" {
			hostToUse = "localhost"
		}

		// Compute effective config port:
		//   1. top-level port (all HTTP)
		//   2. ssePort overrides for SSE requests
		//   3. protocol-level port overrides per protocol
		configPort := effectiveHTTPConfigPort(tc, h.cfg.Test)

		cfg := pkg.SimulationConfig{
			APITimeout:      h.cfg.Test.APITimeout,
			ConfigPort:      configPort,
			KeployPath:      h.cfg.Path,
			ConfigHost:      hostToUse,
			URLReplacements: urlReplacements,
			PortMappings:    portMappings,
		}

		// Check if this is a streaming test case
		if pkg.IsHTTPStreamingTestCase(tc) {
			h.logger.Debug("Simulating HTTP streaming request", zap.Any("Test case", tc.Name))
			resp, err := pkg.SimulateHTTPStreaming(ctx, tc, testSetID, h.logger, cfg)

			if afterErr := h.instrumentation.AfterSimulate(ctx, tc.Name, testSetID); afterErr != nil {
				h.logger.Error("failed to call AfterSimulate hook", zap.Error(afterErr))
			}

			return resp, err
		}

		h.logger.Debug("Simulating HTTP request", zap.Any("Test case", tc))
		resp, err := pkg.SimulateHTTP(ctx, tc, testSetID, h.logger, cfg)

		if err := h.instrumentation.AfterSimulate(ctx, tc.Name, testSetID); err != nil {
			h.logger.Error("failed to call AfterSimulate hook", zap.Error(err))
		}

		return resp, err
	case models.GRPC_EXPORT:

		if err := h.instrumentation.BeforeSimulate(ctx, &tc.GrpcReq.Timestamp, testSetID, tc.Name); err != nil {
			h.logger.Error("failed to call BeforeSimulate hook", zap.Error(err))
		}

		h.logger.Debug("Simulating gRPC request", zap.Any("Test case", tc))
		hostToUse := h.cfg.Test.Host
		if hostToUse == "" {
			hostToUse = "localhost"
		}

		configPort := h.cfg.Test.GRPCPort
		if ps, ok := h.cfg.Test.Protocol["grpc"]; ok && ps.Port > 0 {
			configPort = ps.Port
		}

		resp, err := pkg.SimulateGRPC(ctx, tc, testSetID, h.logger, pkg.SimulationConfig{
			ConfigPort:      configPort,
			ConfigHost:      hostToUse,
			URLReplacements: urlReplacements,
			PortMappings:    portMappings,
		})

		if err := h.instrumentation.AfterSimulate(ctx, tc.Name, testSetID); err != nil {
			h.logger.Error("failed to call AfterSimulate hook", zap.Error(err))
		}

		return resp, err

	default:
		return nil, fmt.Errorf("unsupported test case kind: %s", tc.Kind)
	}

}

func effectiveHTTPConfigPort(tc *models.TestCase, cfg config.Test) uint32 {
	configPort := cfg.Port

	// Header-based SSE detection works for actual SSE streams but fails for CORS preflights
	// (OPTIONS), which usually don't have "text/event-stream" headers.
	isSSE := pkg.IsSSERequest(tc)

	// If this request was recorded on the configured SSE port, treat it as SSE even if it
	// doesn't look like SSE based on headers (e.g., OPTIONS preflight).
	if !isSSE && tc != nil && tc.AppPort > 0 && cfg.SSEPort > 0 && uint32(tc.AppPort) == cfg.SSEPort {
		isSSE = true
	}

	if isSSE {
		if cfg.SSEPort > 0 {
			configPort = cfg.SSEPort
		}
		if ps, ok := cfg.Protocol["sse"]; ok && ps.Port > 0 {
			configPort = ps.Port
		}
	} else {
		if ps, ok := cfg.Protocol["http"]; ok && ps.Port > 0 {
			configPort = ps.Port
		}
	}

	return configPort
}

// mergeReplaceWith extracts and merges URL replacements and port mappings
// from global and per-test-set replaceWith configuration.
func (h *Hooks) mergeReplaceWith(testSetID string) (map[string]string, map[uint32]uint32) {
	rw := h.cfg.Test.ReplaceWith
	hasData := len(rw.Global.URL) > 0 || len(rw.Global.Port) > 0 || len(rw.TestSets) > 0
	if !hasData {
		return nil, nil
	}

	urlReplacements := make(map[string]string)
	portMappings := make(map[uint32]uint32)

	// Start with global replacements
	for k, v := range rw.Global.URL {
		urlReplacements[k] = v
	}
	for k, v := range rw.Global.Port {
		portMappings[k] = v
	}

	// Override/add with per-test-set replacements
	if tsRW, ok := rw.TestSets[testSetID]; ok {
		for k, v := range tsRW.URL {
			urlReplacements[k] = v
		}
		for k, v := range tsRW.Port {
			portMappings[k] = v
		}
	}

	if len(urlReplacements) == 0 {
		urlReplacements = nil
	}
	if len(portMappings) == 0 {
		portMappings = nil
	}
	return urlReplacements, portMappings
}

func (h *Hooks) BeforeTestRun(ctx context.Context, testRunID string) error {
	h.logger.Debug("BeforeTestRun hook executed", zap.String("testRunID", testRunID))

	if err := h.instrumentation.BeforeTestRun(ctx, testRunID); err != nil {
		h.logger.Error("failed to call BeforeTestRun hook", zap.Error(err))
	}

	return nil
}

func (h *Hooks) BeforeTestSetCompose(ctx context.Context, testRunID string, firstRun bool) error {
	h.logger.Debug("BeforeTestSetCompose hook executed", zap.String("testRunID", testRunID))

	if err := h.instrumentation.BeforeTestSetCompose(ctx, testRunID, firstRun); err != nil {
		h.logger.Error("failed to call BeforeTestSetCompose hook", zap.Error(err))
	}

	return nil
}

func (h *Hooks) BeforeTestSetReplay(ctx context.Context, testSetID string) error {
	h.logger.Debug("BeforeTestSetReplay hook executed", zap.String("testSetID", testSetID))
	return nil
}

func (h *Hooks) BeforeTestResult(ctx context.Context, testRunID string, testSetID string, testCaseResults []models.TestResult) error {
	h.logger.Debug("BeforeTestResult called")
	return nil
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

	token, err := h.auth.GetToken(ctx)
	if err != nil || token == "" {
		h.logger.Error("Failed to Authenticate user, skipping mock upload", zap.Error(err))
		return nil
	}
	h.mock.setToken(token)

	err = h.mock.upload(ctx, testSetID)
	if err != nil {
		h.logger.Error("Failed to upload mock, hence skipping", zap.String("testSetID", testSetID), zap.Error(err))
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

	token, err := h.auth.GetToken(ctx)
	if err != nil {
		h.logger.Debug("Failed to Authenticate user, continuing with local mock if present", zap.String("reason", err.Error()))
		return nil
	}
	h.mock.setToken(token)

	// Check if test-set config is present
	tsConfig, err := h.tsConfigDB.Read(ctx, testSetID)
	if err != nil || tsConfig == nil || tsConfig.MockRegistry == nil {
		h.logger.Debug("test set config for upload mock not found, continuing with local mock", zap.String("testSetID", testSetID), zap.Error(err))
		return nil
	}

	if tsConfig.MockRegistry.Mock == "" {
		h.logger.Debug("Mock is empty in test-set config, continuing with local mock if present", zap.String("testSetID", testSetID))
		return nil
	}

	if tsConfig.MockRegistry.App == "" {
		h.logger.Debug("App name is empty in test-set config, continuing with local mock if present", zap.String("testSetID", testSetID))
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
		h.logger.Info("App name in the keploy.yml does not match with the app name in the config.yml in the test-set", zap.String("test-set-config-AppName", tsConfig.MockRegistry.App), zap.String("global-config-Appname", h.cfg.AppName))
		h.logger.Debug("Using app name from the test-set's config.yml for mock retrieval", zap.String("appName", tsConfig.MockRegistry.App))
	}

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

func (h *Hooks) AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error {
	h.logger.Debug("AfterTestRun hook executed", zap.String("testRunID", testRunID), zap.Any("testSetIDs", testSetIDs), zap.Any("coverage", coverage))

	if err := h.instrumentation.AfterTestRun(ctx, testRunID, testSetIDs, coverage); err != nil {
		h.logger.Error("failed to call AfterTestRun hook", zap.Error(err))
	}
	return nil
}

func (h *Hooks) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	consumedMocks, err := h.instrumentation.GetConsumedMocks(ctx)
	if err != nil {
		h.logger.Error("failed to get consumed mocks", zap.Error(err))
		return nil, err
	}
	return consumedMocks, nil
}

// GetNoisyTestCaseNames is a no-op in the default Hooks implementation.
// Callers that embed custom TestHooks should override this to return the
// noisy test case names collected during BeforeTestResult processing.
func (h *Hooks) GetNoisyTestCaseNames(testSetID string) []string {
	return nil
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
