package mock

import (
	"context"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// mockService implements Service for the `keploy mock record|replay` flow.
type mockService struct {
	logger          *zap.Logger
	instrumentation Instrumentation
	mockDB          MockDB
	mappingDB       MappingDB // may be nil (suite-level only)
	store           Store
	hooks           record.RecordHooks // reused so enterprise obfuscation/encryption applies on record
	config          *config.Config
}

// New constructs the mock record/replay service. mappingDB and hooks may be nil
// (a nil hooks becomes a no-op; a nil mappingDB disables per-test scoping).
// store must be non-nil — pass FileStore for OSS.
func New(
	logger *zap.Logger,
	instrumentation Instrumentation,
	mockDB MockDB,
	mappingDB MappingDB,
	store Store,
	hooks record.RecordHooks,
	cfg *config.Config,
) Service {
	if hooks == nil {
		hooks = record.BaseRecordHooks{}
	}
	if store == nil {
		store = FileStore{}
	}
	return &mockService{
		logger:          logger,
		instrumentation: instrumentation,
		mockDB:          mockDB,
		mappingDB:       mappingDB,
		store:           store,
		hooks:           hooks,
		config:          cfg,
	}
}

// Overridable lets a downstream build (enterprise) swap the store and record
// hooks on a constructed mock service — the same post-construction override
// pattern Recorder.SetRecordHooks uses. The CLI's mock command type-asserts to
// this so enterprise can inject a registry-backed store and secret-obfuscation
// hooks without a mock-specific constructor.
type Overridable interface {
	SetStore(store Store)
	SetRecordHooks(hooks record.RecordHooks)
}

// SetStore replaces the mock-set store (e.g. enterprise's registry-backed store).
func (m *mockService) SetStore(store Store) {
	if store != nil {
		m.store = store
	}
}

// SetRecordHooks replaces the record hooks (e.g. enterprise secret obfuscation).
func (m *mockService) SetRecordHooks(hooks record.RecordHooks) {
	if hooks != nil {
		m.hooks = hooks
	}
}

// setName returns the configured mock-set name, defaulting to "default".
func (m *mockService) setName() string {
	name := m.config.Mock.Name
	if name == "" {
		return "default"
	}
	return name
}

// notifyShutdown tells the agent the session is ending so connection errors are
// logged at debug level. Bounded so an unresponsive agent can't hang teardown.
func (m *mockService) notifyShutdown() {
	notifyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
		m.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
	}
}

// propagateExit mirrors the wrapped runner's exit status onto keploy's own
// process exit code (utils.ErrCode), so a wrapped `pytest`/`go test` failure
// fails the keploy process — the contract every CI job depends on. A clean
// runner exit leaves ErrCode at 0. phase is "record" or "replay" for logging.
func (m *mockService) propagateExit(appErr models.AppError, phase string) {
	switch appErr.AppErrorType {
	case models.ErrAppStopped:
		// Clean exit (code 0). Success.
		m.logger.Info("test command finished successfully", zap.String("phase", phase))
	case models.ErrCtxCanceled, "":
		// User interrupt or nothing to report — leave ErrCode untouched.
	case models.ErrUnExpected, models.ErrCommandError:
		code := appErr.ExitCode
		if code <= 0 {
			code = 1
		}
		utils.ErrCode = code
		m.logger.Info("test command exited non-zero; mirroring its exit code",
			zap.String("phase", phase), zap.Int("exitCode", code))
	default:
		utils.ErrCode = 1
		m.logger.Info("test command did not complete cleanly", zap.String("phase", phase), zap.String("reason", string(appErr.AppErrorType)))
	}
}
