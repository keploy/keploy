package log

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestDebugModules(t *testing.T) {
	LogCfg = zap.NewDevelopmentConfig()
	LogCfg.EncoderConfig.EncodeTime = customTimeEncoder
	LogCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	LogCfg.EncoderConfig.EncodeDuration = zapcore.StringDurationEncoder
	LogCfg.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder
	LogCfg.EncoderConfig.EncodeName = nil

	var buf bytes.Buffer
	SetConsoleWriter(&buf)

	tests := []struct {
		name             string
		include          []string
		exclude          []string
		debugMode        bool // true = include ignored, exclude works; false = include works, exclude works only with include
		logOperations    func(logger *zap.Logger)
		expectedOutput   []string
		unexpectedOutput []string
	}{
		// ====== debugMode=false: include works, exclude works ONLY when include is present ======
		{
			name:      "debug=false: include only - single module 'proxy'",
			include:   []string{"proxy"},
			exclude:   []string{},
			debugMode: false,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("This is a proxy debug log")
				logger.Named("record").Debug("This is a record debug log")
			},
			expectedOutput:   []string{"This is a proxy debug log"},
			unexpectedOutput: []string{"This is a record debug log"},
		},
		{
			name:      "debug=false: include only - multiple modules",
			include:   []string{"proxy", "record"},
			exclude:   []string{},
			debugMode: false,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("This is a proxy debug log")
				logger.Named("record").Debug("This is a record debug log")
				logger.Named("test").Debug("This is a test debug log")
			},
			expectedOutput:   []string{"This is a proxy debug log", "This is a record debug log"},
			unexpectedOutput: []string{"This is a test debug log"},
		},
		{
			name:      "debug=false: include only - nested module does not match parent",
			include:   []string{"proxy.http"},
			exclude:   []string{},
			debugMode: false,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("This is a proxy.http debug log")
				logger.Named("proxy").Debug("This is a proxy debug log")
			},
			expectedOutput:   []string{"This is a proxy.http debug log"},
			unexpectedOutput: []string{"This is a proxy debug log"},
		},
		{
			name:      "debug=false: include only - parent enables child",
			include:   []string{"proxy"},
			exclude:   []string{},
			debugMode: false,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("This is a proxy.http debug log")
			},
			expectedOutput:   []string{"This is a proxy.http debug log"},
			unexpectedOutput: []string{},
		},
		{
			name:      "debug=false: empty include - no debug logs",
			include:   []string{},
			exclude:   []string{},
			debugMode: false,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Should not appear")
				logger.Named("hooks").Debug("Should not appear either")
			},
			expectedOutput:   []string{},
			unexpectedOutput: []string{"Should not appear", "Should not appear either"},
		},
		{
			name:      "debug=false: empty include with exclude - exclude ignored (no include = no logs)",
			include:   []string{},
			exclude:   []string{"telemetry"},
			debugMode: false,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Should not appear")
				logger.Named("telemetry").Debug("Should not appear either")
			},
			expectedOutput:   []string{},
			unexpectedOutput: []string{"Should not appear", "Should not appear either"},
		},
		{
			name:      "debug=false: include + exclude - exclude filters from included set",
			include:   []string{"proxy"},
			exclude:   []string{"proxy.mysql"},
			debugMode: false,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("proxy.http should appear")
				logger.Named("proxy.mysql").Debug("proxy.mysql should NOT appear")
				logger.Named("hooks").Debug("hooks should NOT appear")
			},
			expectedOutput:   []string{"proxy.http should appear"},
			unexpectedOutput: []string{"proxy.mysql should NOT appear", "hooks should NOT appear"},
		},
		{
			name:      "debug=false: include + exclude - multiple modules",
			include:   []string{"proxy", "hooks"},
			exclude:   []string{"proxy.mysql", "hooks.conn"},
			debugMode: false,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("proxy.http appears")
				logger.Named("proxy.mysql").Debug("proxy.mysql excluded")
				logger.Named("hooks").Debug("hooks appears")
				logger.Named("hooks.conn").Debug("hooks.conn excluded")
				logger.Named("telemetry").Debug("telemetry not included")
			},
			expectedOutput:   []string{"proxy.http appears", "hooks appears"},
			unexpectedOutput: []string{"proxy.mysql excluded", "hooks.conn excluded", "telemetry not included"},
		},

		// ====== debugMode=true: include IGNORED, only exclude works ======
		{
			name:      "debug=true: no exclude - all modules log",
			include:   []string{},
			exclude:   []string{},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Proxy log")
				logger.Named("hooks").Debug("Hooks log")
				logger.Named("telemetry").Debug("Telemetry log")
			},
			expectedOutput:   []string{"Proxy log", "Hooks log", "Telemetry log"},
			unexpectedOutput: []string{},
		},
		{
			name:      "debug=true: exclude single module",
			include:   []string{},
			exclude:   []string{"telemetry"},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Proxy should appear")
				logger.Named("telemetry").Debug("Telemetry should NOT appear")
			},
			expectedOutput:   []string{"Proxy should appear"},
			unexpectedOutput: []string{"Telemetry should NOT appear"},
		},
		{
			name:      "debug=true: exclude multiple modules",
			include:   []string{},
			exclude:   []string{"telemetry", "auth"},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Proxy appears")
				logger.Named("telemetry").Debug("Telemetry excluded")
				logger.Named("auth").Debug("Auth excluded")
			},
			expectedOutput:   []string{"Proxy appears"},
			unexpectedOutput: []string{"Telemetry excluded", "Auth excluded"},
		},
		{
			name:      "debug=true: exclude hierarchical matching",
			include:   []string{},
			exclude:   []string{"proxy"},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("proxy excluded")
				logger.Named("proxy.http").Debug("proxy.http excluded")
				logger.Named("hooks").Debug("hooks appears")
			},
			expectedOutput:   []string{"hooks appears"},
			unexpectedOutput: []string{"proxy excluded", "proxy.http excluded"},
		},
		{
			name:      "debug=true: include is IGNORED",
			include:   []string{"proxy"}, // This should be IGNORED
			exclude:   []string{},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Proxy appears")
				logger.Named("hooks").Debug("Hooks also appears (include ignored)")
			},
			expectedOutput:   []string{"Proxy appears", "Hooks also appears"},
			unexpectedOutput: []string{},
		},
		{
			name:      "debug=true: include ignored, exclude works",
			include:   []string{"proxy"}, // IGNORED
			exclude:   []string{"telemetry"},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Proxy appears")
				logger.Named("hooks").Debug("Hooks appears (include ignored)")
				logger.Named("telemetry").Debug("Telemetry excluded")
			},
			expectedOutput:   []string{"Proxy appears", "Hooks appears"},
			unexpectedOutput: []string{"Telemetry excluded"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()

			logger, err := SetDebugModules(tt.include, tt.exclude, tt.debugMode)
			assert.NoError(t, err)

			tt.logOperations(logger)

			output := buf.String()
			for _, expected := range tt.expectedOutput {
				assert.Contains(t, output, expected, "Expected log message not found")
			}
			for _, unexpected := range tt.unexpectedOutput {
				assert.NotContains(t, output, unexpected, "Unexpected log message found")
			}
		})
	}
}
