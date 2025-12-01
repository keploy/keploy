package log

import (
    "bytes"
    "strings"
    "testing"

    "go.uber.org/zap"
    "go.uber.org/zap/zapcore"
)

func TestModuleLoggerFactory_GlobalDebugEnabled(t *testing.T) {
    logger, _ := zap.NewDevelopment()
    factory := NewModuleLoggerFactory(logger, true, nil)

    if ! factory.IsDebugEnabled("any-module") {
        t.Error("Expected debug enabled for all modules when globalDebug is true")
    }
}

func TestModuleLoggerFactory_ModuleSpecificDebug(t *testing.T) {
    logger, _ := zap. NewDevelopment()
    moduleDebug := map[string]bool{
        ModuleReplay: true,
        ModuleRecord: false,
    }
    factory := NewModuleLoggerFactory(logger, false, moduleDebug)

    if !factory.IsDebugEnabled(ModuleReplay) {
        t. Error("Expected debug enabled for replay module")
    }
    if factory.IsDebugEnabled(ModuleRecord) {
        t. Error("Expected debug disabled for record module")
    }
    if factory.IsDebugEnabled("unknown") {
        t. Error("Expected debug disabled for unknown module")
    }
}

func TestModuleLoggerFactory_GetLogger_FiltersDebugWhenDisabled(t *testing.T) {
    // Create a buffer to capture log output
    var buf bytes.Buffer
    
    encoder := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
    core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore. DebugLevel)
    logger := zap.New(core)

    // Create factory with debug disabled for "test" module
    factory := NewModuleLoggerFactory(logger, false, map[string]bool{"test": false})
    
    moduleLogger := factory.GetLogger("test")
    moduleLogger.Debug("this should be filtered")
    moduleLogger.Info("this should appear")

    output := buf.String()
    if strings.Contains(output, "this should be filtered") {
        t.Error("Debug message should have been filtered")
    }
    if ! strings.Contains(output, "this should appear") {
        t.Error("Info message should appear")
    }
}

func TestModuleLoggerFactory_GetLogger_ShowsDebugWhenEnabled(t *testing.T) {
    var buf bytes.Buffer
    
    encoder := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
    core := zapcore.NewCore(encoder, zapcore. AddSync(&buf), zapcore.DebugLevel)
    logger := zap. New(core)

    // Create factory with debug enabled for "test" module
    factory := NewModuleLoggerFactory(logger, false, map[string]bool{"test": true})
    
    moduleLogger := factory. GetLogger("test")
    moduleLogger. Debug("this should appear")

    output := buf. String()
    if !strings.Contains(output, "this should appear") {
        t.Error("Debug message should appear when module debug is enabled")
    }
}