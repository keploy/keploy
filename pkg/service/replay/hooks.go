// Package replay provides the hooks for the replay service
package replay

import (
	"context"
	"crypto/tls"
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
	// tlsConfig, when non-nil, is forwarded to pkg.SimulationConfig.TLSConfig
	// so the replay HTTP transport pins a specific cert (e.g. cluster-mode
	// replay against a short-lived pod with a self-signed keystore).
	tlsConfig *tls.Config
}

// SetReplayTLSConfig installs a *tls.Config that the replay HTTP client
// uses for HTTPS test cases. Call before SimulateRequest. nil resets to
// stdlib system-pool default.
func (h *Hooks) SetReplayTLSConfig(c *tls.Config) {
	h.tlsConfig = c
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
	urlReplacements, portMappings := mergeReplaceWith(h.cfg.Test, testSetID)

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
			TLSConfig:       h.tlsConfig,
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
			// Bounds the response drain. Without it a bidi stream the server
			// holds open hangs the whole test set instead of failing one case.
			APITimeout:      h.cfg.Test.APITimeout,
			ConfigPort:      configPort,
			ConfigHost:      hostToUse,
			URLReplacements: urlReplacements,
			PortMappings:    portMappings,
		})

		if err := h.instrumentation.AfterSimulate(ctx, tc.Name, testSetID); err != nil {
			h.logger.Error("failed to call AfterSimulate hook", zap.Error(err))
		}

		return resp, err

	case models.CONSUMER:
		return h.simulateConsumer(ctx, tc, testSetID)

	default:
		return nil, fmt.Errorf("unsupported test case kind: %s", tc.Kind)
	}

}

// simulateConsumer is SimulateRequest for a Kind: Consumer test case. It is
// ARM-AND-AWAIT, not request-and-response: there is nothing to send, because
// the "request" is a message the recorded broker delivered to the worker.
//
// EVERY FAILURE PATH HERE RETURNS A REFUSAL RESULT, NOT AN ERROR. Returning an
// error would route the test through CreateFailedTestResult, which builds a
// result with no consumer verdict and no failure category on it — so the run
// would go red with nothing naming why, which is only marginally better than
// going green. A *models.ConsumerResult carrying a Refusal flows through the
// normal judge instead and comes out as a FAILED test with a named category, a
// rendered row and a non-zero exit. The one thing that is NEVER produced on
// any of these paths is a pass.
func (h *Hooks) simulateConsumer(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	if tc.ConsumerSpec == nil {
		return &models.ConsumerResult{
			TestID:        tc.Name,
			Refusal:       models.CategoryConsumerUnsupportedSpec,
			EndReason:     models.ConsumerEndReasonInternalError,
			RefusalDetail: "this test case is Kind: Consumer but carries no consumer spec, so there is no trigger to deliver",
		}, nil
	}

	ci, ok := h.instrumentation.(ConsumerInstrumentation)
	if !ok {
		h.logger.Error("the running agent does not implement consumer instrumentation",
			zap.String("testcase", tc.Name),
			zap.String("testset", testSetID),
			zap.String("category", string(models.CategoryConsumerUnsupportedAgent)),
			zap.String("next_step", "consumer replay needs an agent build that can arm a delivery window; upgrade the agent, or remove the consumer test cases from this set"))
		return &models.ConsumerResult{
			TestID:        tc.Name,
			Refusal:       models.CategoryConsumerUnsupportedAgent,
			EndReason:     models.ConsumerEndReasonInternalError,
			ExpectEffects: tc.ConsumerSpec.Completion.ExpectEffects,
			RefusalDetail: "the running agent does not implement consumer instrumentation, so no delivery window could be opened and the worker was never given the recorded message",
		}, nil
	}

	spec := tc.ConsumerSpec
	arm := models.ConsumerArm{
		TestID:     tc.Name,
		TestSetID:  testSetID,
		Protocol:   spec.Protocol,
		Trigger:    spec.Trigger,
		Completion: spec.Completion,
	}

	h.logger.Debug("arming consumer trigger",
		zap.String("testcase", tc.Name),
		zap.String("testset", testSetID),
		zap.String("protocol", spec.Protocol),
		zap.String("target", spec.Trigger.Target),
		zap.Int("expectEffects", spec.Completion.ExpectEffects))

	if err := ci.ArmConsumerTrigger(ctx, arm); err != nil {
		h.logger.Error("failed to arm the consumer trigger", zap.Error(err),
			zap.String("testcase", tc.Name),
			zap.String("testset", testSetID))
		return &models.ConsumerResult{
			TestID:        tc.Name,
			Refusal:       models.CategoryConsumerTriggerNotDelivered,
			EndReason:     models.ConsumerEndReasonTriggerNotDelivered,
			ExpectEffects: spec.Completion.ExpectEffects,
			RefusalDetail: "the recorded message could not be handed to the application: " + err.Error(),
		}, nil
	}

	res, err := ci.AwaitConsumerEffects(ctx, tc.Name)
	if err != nil {
		h.logger.Error("failed to await the consumer effects", zap.Error(err),
			zap.String("testcase", tc.Name),
			zap.String("testset", testSetID))
		return &models.ConsumerResult{
			TestID:        tc.Name,
			Refusal:       models.CategoryConsumerUnsupportedAgent,
			EndReason:     models.ConsumerEndReasonInternalError,
			ExpectEffects: spec.Completion.ExpectEffects,
			RefusalDetail: "the delivery window could not be read back from the agent: " + err.Error(),
		}, nil
	}
	if res == nil {
		return &models.ConsumerResult{
			TestID:        tc.Name,
			Refusal:       models.CategoryConsumerUnsupportedAgent,
			EndReason:     models.ConsumerEndReasonInternalError,
			ExpectEffects: spec.Completion.ExpectEffects,
			RefusalDetail: "the agent reported no delivery window for this test",
		}, nil
	}
	return res, nil
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
// from global and per-test-set replaceWith configuration. It is a free function
// (reading only config.Test) so the reset-resend readiness probe can resolve the
// same effective dial target the simulation uses — see resolveProbeTarget.
func mergeReplaceWith(testCfg config.Test, testSetID string) (map[string]string, map[uint32]uint32) {
	rw := testCfg.ReplaceWith
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

func (h *Hooks) BeforeTestSetCompose(ctx context.Context, testRunID string, testSetID string, firstRun bool) error {
	h.logger.Debug("BeforeTestSetCompose hook executed", zap.String("testRunID", testRunID), zap.String("testSetID", testSetID))

	// Deliberately no log.RotateDebugFileForTestSet here. The CLI's
	// debug log stays at a single <cfg.Path>/cloud-debug.log for the
	// whole run; only the agent's debug log rotates per test set
	// (handled by HandleBeforeTestSetCompose in the agent process).

	if err := h.instrumentation.BeforeTestSetCompose(ctx, testRunID, testSetID, firstRun); err != nil {
		h.logger.Error("failed to call BeforeTestSetCompose hook", zap.Error(err))
	}

	return nil
}

// BeforeTestSetReplay deliberately does NOT touch the consumer delivery gate.
//
// It used to: it type-asserted the instrumentation and reset the gate on every
// test set of every run. The shipping instrumentation (*platform/http.
// AgentClient) implements the interface unconditionally, so that assertion
// always succeeded and a pure HTTP suite with no consumer test anywhere issued
// one extra agent round trip per test set. This hook has no test cases in
// scope and so cannot tell whether a set contains a consumer test at all; the
// reset lives in Replayer.resetConsumerGate instead, next to the end-of-set
// drain, where containsConsumerTest can short-circuit it.
func (h *Hooks) BeforeTestSetReplay(_ context.Context, testSetID string) error {
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
