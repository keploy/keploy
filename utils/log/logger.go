// Package log provides utility functions for logging.
package log

import (
	"bytes"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

var Emoji = "\U0001F430" + " Keploy:"

var LogCfg zap.Config

// Redactor rewrites log entries/fields in place to strip secrets before the
// underlying zap core writes them out. Implementations live outside this
// package (enterprise plugs in one that uses its secret detector) so OSS
// keploy stays free of product-specific redaction rules.
//
// There are two redaction hooks because zap fields come in two flavours:
//   - strings we can inspect directly (zap.String, the common case) — those
//     go through RedactEntry / RedactField before encoding, which lets us
//     redact by field NAME as well as value.
//   - everything else (zap.Any over http.Header, protocol structs, byte
//     packets, reflect-marshaled values) — those only exist as text after
//     zap's encoder runs. RedactEncoded operates on that final text, so it
//     catches anything the field-level pass couldn't reach.
//
// Implementations MUST be safe for concurrent use — the methods are called
// on the log hot path from any goroutine.
type Redactor interface {
	RedactEntry(ent *zapcore.Entry)
	RedactField(f *zapcore.Field)
	RedactEncoded(text string) string
}

// redactorHolder wraps Redactor so atomic.Value always stores the same
// concrete type (atomic.Value panics on type changes across Stores).
type redactorHolder struct{ r Redactor }

var globalRedactor atomic.Value

// SetRedactor registers r as the active redactor for all loggers built by
// this package. Pass nil to disable. Safe to call at any time; only log
// lines emitted after the call are affected.
func SetRedactor(r Redactor) {
	globalRedactor.Store(redactorHolder{r: r})
}

func loadRedactor() Redactor {
	v := globalRedactor.Load()
	if v == nil {
		return nil
	}
	return v.(redactorHolder).r
}

// redactingCore wraps a zapcore.Core and runs the active Redactor over every
// entry and field before delegating to the inner core. The indirection
// through loadRedactor() means SetRedactor can be called before or after
// logger construction — loggers built with a nil redactor at boot time
// still honor a redactor registered later.
type redactingCore struct {
	zapcore.Core
}

func newRedactingCore(c zapcore.Core) zapcore.Core {
	return &redactingCore{Core: c}
}

func (c *redactingCore) With(fields []zapcore.Field) zapcore.Core {
	if r := loadRedactor(); r != nil {
		for i := range fields {
			r.RedactField(&fields[i])
		}
	}
	return &redactingCore{Core: c.Core.With(fields)}
}

// Check must be overridden so the CheckedEntry routes Write back through
// our wrapper rather than the embedded core directly. Without this, the
// embedded ioCore's Check would register itself and bypass redaction.
func (c *redactingCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *redactingCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	if r := loadRedactor(); r != nil {
		r.RedactEntry(&ent)
		for i := range fields {
			r.RedactField(&fields[i])
		}
	}
	return c.Core.Write(ent, fields)
}

// redactingWriter wraps a zapcore.WriteSyncer and runs the active Redactor's
// RedactEncoded pass on every byte slice before it reaches the sink. This is
// the last line of defence for fields that zap encodes via reflection
// (zap.Any, zap.Binary, zap.ByteString) — the field-level pass on Core.Write
// never sees those as strings, but by the time zap calls sink.Write the
// whole log line is a single formatted byte slice we can scan.
//
// Wrapping at the writer level (rather than the encoder) is deliberate: it
// works the same regardless of which encoder built the line, so the console
// path, the JSON path (ChangeColorEncoding), and any future encoder choice
// all get the same post-serialization scrub.
type redactingWriter struct {
	inner zapcore.WriteSyncer
}

func wrapWriter(w zapcore.WriteSyncer) zapcore.WriteSyncer {
	return &redactingWriter{inner: w}
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	r := loadRedactor()
	if r == nil {
		return w.inner.Write(p)
	}
	redacted := r.RedactEncoded(string(p))
	// Report bytes-written against the original length so zap's accounting
	// (and anything downstream that compares against len(p)) stays consistent
	// even when redaction changes the encoded length. Redact() itself is
	// length-preserving per character, but when a value gets replaced with
	// an obfuscated substitute of the same byte length, the overall line
	// length doesn't change; still, guard against any future scanner that
	// might add or drop bytes.
	if _, err := w.inner.Write([]byte(redacted)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *redactingWriter) Sync() error {
	return w.inner.Sync()
}

// ANSI-friendly console encoder
type ansiConsoleEncoder struct {
	*zapcore.EncoderConfig
	zapcore.Encoder
}

func NewANSIConsoleEncoder(cfg zapcore.EncoderConfig) zapcore.Encoder {
	return ansiConsoleEncoder{
		EncoderConfig: &cfg,
		Encoder:       zapcore.NewConsoleEncoder(cfg),
	}
}

func (e ansiConsoleEncoder) EncodeEntry(ent zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	buf, err := e.Encoder.EncodeEntry(ent, fields)
	if err != nil {
		return nil, err
	}

	// Convert escaped unicode sequences back to raw ANSI codes
	bytes := buf.Bytes()
	bytes = replaceAll(bytes, []byte("\\u001b"), []byte("\u001b"))
	bytes = replaceAll(bytes, []byte("\\u001B"), []byte("\u001b"))

	buf.Reset()
	buf.AppendString(string(bytes))
	return buf, nil
}

// replaceAll replaces all occurrences of old with new in the byte slice
func replaceAll(s, old, new []byte) []byte {
	return bytes.Replace(s, old, new, -1)
}

func (e ansiConsoleEncoder) Clone() zapcore.Encoder {
	return ansiConsoleEncoder{
		EncoderConfig: e.EncoderConfig,
		Encoder:       e.Encoder.Clone(),
	}
}

func New() (*zap.Logger, *os.File, error) {
	// Register the ANSI-friendly encoder
	_ = zap.RegisterEncoder("ansiConsole", func(config zapcore.EncoderConfig) (zapcore.Encoder, error) {
		return NewANSIConsoleEncoder(config), nil
	})

	logFile, err := os.OpenFile("keploy-logs.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0777)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open log file: %v", err)
	}

	err = os.Chmod("keploy-logs.txt", 0777)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set the log file permission to 777: %v", err)
	}

	writer := wrapWriter(zapcore.NewMultiWriteSyncer(zapcore.AddSync(os.Stdout), zapcore.AddSync(logFile)))

	LogCfg = zap.NewDevelopmentConfig()
	LogCfg.Encoding = "ansiConsole" // Use our custom encoder

	// Customize the encoder config
	LogCfg.EncoderConfig.EncodeTime = customTimeEncoder
	LogCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	LogCfg.EncoderConfig.EncodeDuration = zapcore.StringDurationEncoder
	LogCfg.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder

	LogCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	LogCfg.DisableStacktrace = true
	LogCfg.EncoderConfig.EncodeCaller = nil

	// Build the core with our custom encoder
	encoder := NewANSIConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		writer,
		LogCfg.Level,
	)

	logger := zap.New(newRedactingCore(core))
	return logger, logFile, nil
}

func ChangeLogLevel(level zapcore.Level) (*zap.Logger, error) {
	LogCfg.Level = zap.NewAtomicLevelAt(level)
	if level == zap.DebugLevel {
		LogCfg.DisableStacktrace = false
		LogCfg.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder
	}

	// Use our custom encoder when building
	encoder := NewANSIConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		wrapWriter(zapcore.AddSync(os.Stdout)),
		LogCfg.Level,
	)

	logger := zap.New(newRedactingCore(core))
	return logger, nil
}

// RedirectToStderr re-creates the logger writing to stderr instead of stdout.
// Use this when --json mode is active to prevent log lines from contaminating
// structured JSON on stdout.
func RedirectToStderr() (*zap.Logger, error) {
	encoder := NewANSIConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		wrapWriter(zapcore.AddSync(os.Stderr)),
		LogCfg.Level,
	)

	logger := zap.New(newRedactingCore(core))
	return logger, nil
}

func AddMode(mode string) (*zap.Logger, error) {
	cfg := LogCfg
	cfg.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		emoji := "\U0001F430"
		modeStr := fmt.Sprintf("Keploy(%s):", mode)
		enc.AppendString(emoji + " " + modeStr + " " + t.Format(time.RFC3339))
	}

	encoder := NewANSIConsoleEncoder(cfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		wrapWriter(zapcore.AddSync(os.Stdout)),
		cfg.Level,
	)

	logger := zap.New(newRedactingCore(core))
	return logger, nil
}

func ChangeColorEncoding() (*zap.Logger, error) {
	// For non-color mode, use the standard console encoder
	LogCfg.Encoding = "console"
	LogCfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder

	logger, err := LogCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build config for logger: %v", err)
	}
	return logger.WithOptions(zap.WrapCore(newRedactingCore)), nil
}
