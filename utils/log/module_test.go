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
		modules          map[string]bool
		logOperations    func(logger *zap.Logger)
		expectedOutput   []string
		unexpectedOutput []string
	}{
		{
			name: "Enable single module 'proxy'",
			modules: map[string]bool{
				"proxy": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("This is a proxy debug log")
				logger.Named("record").Debug("This is a record debug log")
			},
			expectedOutput:   []string{"This is a proxy debug log"},
			unexpectedOutput: []string{"This is a record debug log"},
		},
		{
			name: "Enable multiple modules 'proxy' and 'record'",
			modules: map[string]bool{
				"proxy":  true,
				"record": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("This is a proxy debug log")
				logger.Named("record").Debug("This is a record debug log")
				logger.Named("test").Debug("This is a test debug log")
			},
			expectedOutput:   []string{"This is a proxy debug log", "This is a record debug log"},
			unexpectedOutput: []string{"This is a test debug log"},
		},
		{
			name: "Enable nested module 'proxy.http'",
			modules: map[string]bool{
				"proxy.http": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("This is a proxy.http debug log")
				logger.Named("proxy").Debug("This is a proxy debug log")
			},
			expectedOutput:   []string{"This is a proxy.http debug log"},
			unexpectedOutput: []string{"This is a proxy debug log"},
		},
		{
			name: "Enable parent module 'proxy' enables child 'proxy.http'",
			modules: map[string]bool{
				"proxy": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.http").Debug("This is a proxy.http debug log")
			},
			expectedOutput:   []string{"This is a proxy.http debug log"},
			unexpectedOutput: []string{},
		},
		{
			name: "Enable single module 'agent'",
			modules: map[string]bool{
				"agent": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("agent").Debug("Agent starting up")
				logger.Named("proxy").Debug("Proxy listening")
			},
			expectedOutput:   []string{"Agent starting up"},
			unexpectedOutput: []string{"Proxy listening"},
		},
		{
			name: "Enable single module 'docker'",
			modules: map[string]bool{
				"docker": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("docker").Debug("Container created")
				logger.Named("test").Debug("Test started")
			},
			expectedOutput:   []string{"Container created"},
			unexpectedOutput: []string{"Test started"},
		},
		{
			name: "Enable 'tools' and 'report'",
			modules: map[string]bool{
				"tools":  true,
				"report": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("tools").Debug("Generating config")
				logger.Named("report").Debug("Report generated")
				logger.Named("record").Debug("Recording traffic")
			},
			expectedOutput:   []string{"Generating config", "Report generated"},
			unexpectedOutput: []string{"Recording traffic"},
		},
		{
			name: "Enable specific db 'test-db'",
			modules: map[string]bool{
				"test-db": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("test-db").Debug("Writing to test db")
				logger.Named("mock-db").Debug("Reading from mock db")
				logger.Named("map-db").Debug("Mapping entry")
			},
			expectedOutput:   []string{"Writing to test db"},
			unexpectedOutput: []string{"Reading from mock db", "Mapping entry"},
		},
		{
			name: "Enable 'proxy.mysql' only",
			modules: map[string]bool{
				"proxy.mysql": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.mysql").Debug("Parsing MySQL packet")
				logger.Named("proxy.http").Debug("Parsing HTTP header")
				logger.Named("proxy.mongo").Debug("Parsing Mongo opcode")
			},
			expectedOutput:   []string{"Parsing MySQL packet"},
			unexpectedOutput: []string{"Parsing HTTP header", "Parsing Mongo opcode"},
		},
		{
			name: "Enable 'proxy.postgres_v1' excludes 'v2'",
			modules: map[string]bool{
				"proxy.postgres_v1": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy.postgres_v1").Debug("Handling PG V1")
				logger.Named("proxy.postgres_v2").Debug("Handling PG V2")
			},
			expectedOutput:   []string{"Handling PG V1"},
			unexpectedOutput: []string{"Handling PG V2"},
		},
		{
			name: "Enable 'record' and 'proxy.grpc'",
			modules: map[string]bool{
				"record":     true,
				"proxy.grpc": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("record").Debug("Recording request")
				logger.Named("proxy.grpc").Debug("GRPC Frame received")
				logger.Named("proxy.http").Debug("HTTP Header received")
			},
			expectedOutput:   []string{"Recording request", "GRPC Frame received"},
			unexpectedOutput: []string{"HTTP Header received"},
		},
		{
			name: "Module present in map but set to false",
			modules: map[string]bool{
				"proxy": false,
				"test":  true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("proxy").Debug("Should not see this")
				logger.Named("test").Debug("Should see this")
			},
			expectedOutput:   []string{"Should see this"},
			unexpectedOutput: []string{"Should not see this"},
		},
		{
			name: "Enable 'telemetry' and 'auth'",
			modules: map[string]bool{
				"telemetry": true,
				"auth":      true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("telemetry").Debug("Sending metrics")
				logger.Named("auth").Debug("User authenticated")
				logger.Named("gen").Debug("Generating tests")
			},
			expectedOutput:   []string{"Sending metrics", "User authenticated"},
			unexpectedOutput: []string{"Generating tests"},
		},
				{
			name: "Enable unknown module",
			modules: map[string]bool{
				"alien-module": true,
			},
			logOperations: func(logger *zap.Logger) {
				logger.Named("alien-module").Debug("Hello from space")
				logger.Named("proxy").Debug("Hello from earth")
			},
			expectedOutput:   []string{"Hello from space"},
			unexpectedOutput: []string{"Hello from earth"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			
			logger, err := SetDebugModules(tt.modules)
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
