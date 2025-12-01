package log

import (
    "go.uber.org/zap"
    "go.uber.org/zap/zapcore"
)

// Module name constants - add modules as needed
const (
    ModuleReplay   = "replay"
    ModuleRecord   = "record"
    ModuleProxy    = "proxy"
    ModuleDocker   = "docker"
    ModuleAgent    = "agent"
    ModuleReport   = "report"
    ModuleTools    = "tools"
    ModuleContract = "contract"
)

// ModuleLoggerFactory creates module-specific loggers
type ModuleLoggerFactory struct {
    baseLogger  *zap.Logger
    globalDebug bool
    moduleDebug map[string]bool
}

// Global instance - initialized during startup
var GlobalLoggerFactory *ModuleLoggerFactory

// NewModuleLoggerFactory creates a new factory
func NewModuleLoggerFactory(baseLogger *zap. Logger, globalDebug bool, moduleDebug map[string]bool) *ModuleLoggerFactory {
    if moduleDebug == nil {
        moduleDebug = make(map[string]bool)
    }
    return &ModuleLoggerFactory{
        baseLogger:  baseLogger,
        globalDebug: globalDebug,
        moduleDebug: moduleDebug,
    }
}

// InitGlobalFactory initializes the global logger factory
func InitGlobalFactory(baseLogger *zap.Logger, globalDebug bool, moduleDebug map[string]bool) {
    GlobalLoggerFactory = NewModuleLoggerFactory(baseLogger, globalDebug, moduleDebug)
}

// GetLogger returns a logger for a specific module with appropriate log level
func (f *ModuleLoggerFactory) GetLogger(moduleName string) *zap.Logger {
    namedLogger := f.baseLogger. Named(moduleName)
    
    if f.IsDebugEnabled(moduleName) {
        // Debug enabled - return logger as-is (will show debug logs)
        return namedLogger
    }
    
    // Debug disabled - wrap with filter to hide debug logs
    return namedLogger.WithOptions(zap. WrapCore(func(core zapcore.Core) zapcore.Core {
        return &levelFilterCore{Core: core, minLevel: zapcore.InfoLevel}
    }))
}

// IsDebugEnabled checks if debug is enabled for a module
func (f *ModuleLoggerFactory) IsDebugEnabled(moduleName string) bool {
    if f.globalDebug {
        return true
    }
    return f.moduleDebug[moduleName]
}

// GetModuleLogger is a convenience function using the global factory
func GetModuleLogger(moduleName string) *zap.Logger {
    if GlobalLoggerFactory == nil {
        // Fallback to default logger if factory not initialized
        logger, _, _ := New()
        return logger. Named(moduleName)
    }
    return GlobalLoggerFactory.GetLogger(moduleName)
}

// levelFilterCore filters logs below minimum level
type levelFilterCore struct {
    zapcore.Core
    minLevel zapcore. Level
}

func (c *levelFilterCore) Enabled(level zapcore.Level) bool {
    return level >= c. minLevel && c.Core. Enabled(level)
}

func (c *levelFilterCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
    if c.Enabled(entry.Level) {
        return c.Core.Check(entry, ce)
    }
    return ce
}

func (c *levelFilterCore) With(fields []zapcore.Field) zapcore.Core {
    return &levelFilterCore{
        Core:     c. Core.With(fields),
        minLevel: c.minLevel,
    }
}