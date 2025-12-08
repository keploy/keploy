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
		debugMode        bool // true = use both include & exclude (Caddy-style), false = use include only
		logOperations    func(logger *zap.Logger)
		expectedOutput   []string
		unexpectedOutput []string
	}{
		// ====== debugMode=false (include only, exclude ignored) tests ======
		{
			name:      "Include mode: single module 'proxy'",
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
			name:      "Include mode: multiple modules 'proxy' and 'record'",
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
			name:      "Include mode: nested module 'proxy.http' does not match parent",
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
			name:      "Include mode: parent module 'proxy' enables child 'proxy.http'",
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
			name:      "Include mode: 'proxy.mysql' only",
			include:   []string{"proxy.mysql"},
			exclude:   []string{},
			debugMode: false,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.mysql").Debug("Parsing MySQL packet")
				logger.Named("proxy.http").Debug("Parsing HTTP header")
				logger.Named("proxy.mongo").Debug("Parsing Mongo opcode")
			},
			expectedOutput:   []string{"Parsing MySQL packet"},
			unexpectedOutput: []string{"Parsing HTTP header", "Parsing Mongo opcode"},
		},
		{
			name:      "Include mode: exclude list is ignored",
			include:   []string{"proxy"},
			exclude:   []string{"proxy"}, // This should be ignored when debugMode=false
			debugMode: false,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Proxy should appear (exclude ignored)")
				logger.Named("record").Debug("Record should not appear")
			},
			expectedOutput:   []string{"Proxy should appear (exclude ignored)"},
			unexpectedOutput: []string{"Record should not appear"},
		},
		{
			name:      "Include mode: empty include blocks all",
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

		// ====== debugMode=true (both include & exclude work, Caddy-style) tests ======
		{
			name:      "Debug mode: exclude only - single module 'proxy'",
			include:   []string{},
			exclude:   []string{"proxy"},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("This should NOT appear")
				logger.Named("record").Debug("This should appear")
			},
			expectedOutput:   []string{"This should appear"},
			unexpectedOutput: []string{"This should NOT appear"},
		},
		{
			name:      "Debug mode: exclude only - hierarchical matching",
			include:   []string{},
			exclude:   []string{"proxy"},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("Excluded child log")
				logger.Named("hooks").Debug("Should appear")
			},
			expectedOutput:   []string{"Should appear"},
			unexpectedOutput: []string{"Excluded child log"},
		},
		{
			name:      "Debug mode: exclude only - multiple modules",
			include:   []string{},
			exclude:   []string{"telemetry", "auth"},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("telemetry").Debug("Should not appear")
				logger.Named("auth").Debug("Also should not appear")
				logger.Named("proxy").Debug("Should appear")
			},
			expectedOutput:   []string{"Should appear"},
			unexpectedOutput: []string{"Should not appear", "Also should not appear"},
		},
		{
			name:      "Debug mode: empty include and exclude allows all",
			include:   []string{},
			exclude:   []string{},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Proxy log")
				logger.Named("hooks").Debug("Hooks log")
			},
			expectedOutput:   []string{"Proxy log", "Hooks log"},
			unexpectedOutput: []string{},
		},
		{
			name:      "Debug mode: include only - filters to specific modules",
			include:   []string{"proxy"},
			exclude:   []string{},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Proxy should appear")
				logger.Named("hooks").Debug("Hooks should NOT appear")
			},
			expectedOutput:   []string{"Proxy should appear"},
			unexpectedOutput: []string{"Hooks should NOT appear"},
		},
		{
			name:      "Debug mode: BOTH include and exclude work together (Caddy-style)",
			include:   []string{"proxy"},
			exclude:   []string{"proxy.mysql"},
			debugMode: true,
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("proxy.http should appear")
				logger.Named("proxy.grpc").Debug("proxy.grpc should appear")
				logger.Named("proxy.mysql").Debug("proxy.mysql should NOT appear (excluded)")
				logger.Named("hooks").Debug("hooks should NOT appear (not in include)")
			},
			expectedOutput:   []string{"proxy.http should appear", "proxy.grpc should appear"},
			unexpectedOutput: []string{"proxy.mysql should NOT appear", "hooks should NOT appear"},
		},
		{
			name:      "Debug mode: include + exclude - exclude filters from included set",
			include:   []string{"proxy", "hooks"},
			exclude:   []string{"proxy.mysql", "hooks.conn"},
			debugMode: true,
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
