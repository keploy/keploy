// Package replay provides the hooks for the replay service
package replay

import (
	"context"
	"fmt"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type Hooks struct {
	logger          *zap.Logger
	cfg             *config.Config
	instrumentation Instrumentation
}

func NewHooks(logger *zap.Logger, cfg *config.Config, instrumentation Instrumentation) TestHooks {
	return &Hooks{
		cfg:             cfg,
		logger:          logger,
		instrumentation: instrumentation,
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
	return nil
}

func (h *Hooks) BeforeTestSetRun(ctx context.Context, testSetID string) error {
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
