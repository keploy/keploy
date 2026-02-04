package replay

import (
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// Logger exposes the replay logger for consumers that share replay runtime state.
func (r *Replayer) Logger() *zap.Logger {
	return r.logger
}

// Config exposes the replay config for consumers that share replay runtime state.
func (r *Replayer) Config() *config.Config {
	return r.config
}

// Instrumentation exposes the instrumentation client for consumers that share replay runtime state.
func (r *Replayer) Instrumentation() Instrumentation {
	return r.instrumentation
}

// MockDB exposes the mock database for consumers that share replay runtime state.
func (r *Replayer) MockDB() MockDB {
	return r.mockDB
}
