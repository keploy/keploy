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
		logOperations    func(logger *zap.Logger)
		expectedOutput   []string
		unexpectedOutput []string
	}{
		{
			name:    "Include single module 'proxy'",
			include: []string{"proxy"},
			exclude: []string{},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("This is a proxy debug log")
				logger.Named("record").Debug("This is a record debug log")
			},
			expectedOutput:   []string{"This is a proxy debug log"},
			unexpectedOutput: []string{"This is a record debug log"},
		},
		{
			name:    "Include multiple modules 'proxy' and 'record'",
			include: []string{"proxy", "record"},
			exclude: []string{},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("This is a proxy debug log")
				logger.Named("record").Debug("This is a record debug log")
				logger.Named("test").Debug("This is a test debug log")
			},
			expectedOutput:   []string{"This is a proxy debug log", "This is a record debug log"},
			unexpectedOutput: []string{"This is a test debug log"},
		},
		{
			name:    "Include nested module 'proxy.http' does not match parent",
			include: []string{"proxy.http"},
			exclude: []string{},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("This is a proxy.http debug log")
				logger.Named("proxy").Debug("This is a proxy debug log")
			},
			expectedOutput:   []string{"This is a proxy.http debug log"},
			unexpectedOutput: []string{"This is a proxy debug log"},
		},
		{
			name:    "Include parent module 'proxy' enables child 'proxy.http'",
			include: []string{"proxy"},
			exclude: []string{},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("This is a proxy.http debug log")
			},
			expectedOutput:   []string{"This is a proxy.http debug log"},
			unexpectedOutput: []string{},
		},
		{
			name:    "Exclude single module 'proxy' - other modules log",
			include: []string{},
			exclude: []string{"proxy"},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("This should NOT appear")
				logger.Named("record").Debug("This should appear")
			},
			expectedOutput:   []string{"This should appear"},
			unexpectedOutput: []string{"This should NOT appear"},
		},
		{
			name:    "Exclude with hierarchical matching",
			include: []string{},
			exclude: []string{"proxy"},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("Excluded child log")
				logger.Named("hooks").Debug("Should appear")
			},
			expectedOutput:   []string{"Should appear"},
			unexpectedOutput: []string{"Excluded child log"},
		},
		{
			name:    "Include takes priority over exclude",
			include: []string{"proxy.http"},
			exclude: []string{"proxy"},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("Include wins")
				logger.Named("proxy.grpc").Debug("Should NOT appear")
			},
			expectedOutput:   []string{"Include wins"},
			unexpectedOutput: []string{"Should NOT appear"},
		},
		{
			name:    "Include 'proxy.mysql' only",
			include: []string{"proxy.mysql"},
			exclude: []string{},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.mysql").Debug("Parsing MySQL packet")
				logger.Named("proxy.http").Debug("Parsing HTTP header")
				logger.Named("proxy.mongo").Debug("Parsing Mongo opcode")
			},
			expectedOutput:   []string{"Parsing MySQL packet"},
			unexpectedOutput: []string{"Parsing HTTP header", "Parsing Mongo opcode"},
		},
		{
			name:    "Empty include and exclude - all debug logs pass",
			include: []string{},
			exclude: []string{},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Proxy log")
				logger.Named("hooks").Debug("Hooks log")
			},
			expectedOutput:   []string{"Proxy log", "Hooks log"},
			unexpectedOutput: []string{},
		},
		{
			name:    "Exclude multiple modules",
			include: []string{},
			exclude: []string{"telemetry", "auth"},
			logOperations: func(logger *zap.Logger) {
				logger.Named("telemetry").Debug("Should not appear")
				logger.Named("auth").Debug("Also should not appear")
				logger.Named("proxy").Debug("Should appear")
			},
			expectedOutput:   []string{"Should appear"},
			unexpectedOutput: []string{"Should not appear", "Also should not appear"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()

			logger, err := SetDebugModules(tt.include, tt.exclude)
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
