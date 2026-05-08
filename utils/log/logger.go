// Package log provides utility functions for logging.
package log

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// SetRedactor registers r as the active redactor for every logger built
// by this package. Pass nil to disable. Safe to call at any time; only
// log lines emitted after the call are affected. Registration is
// process-global by design — keploy daemonizes one logger and there is
// no per-logger or per-test scoping. If you need that later, the right
// move is to attach the redactor to the core/writer wrappers at
// construction time rather than reading it from a package var.
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

// Inner returns the wrapped core. Used by callers that need to compose
// new cores around the same redaction-aware structure (e.g. teeing on a
// second sink with a different level filter) without double-wrapping
// redaction.
func (c *redactingCore) Inner() zapcore.Core {
	return c.Core
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
	// RedactEncoded is byte-length-preserving (Redact substitutes within
	// the same character class), so the redacted slice has len(p). We
	// transform p, hand the result to the sink, and report success in
	// terms of p — the io.Writer contract is "wrote n bytes from p"; the
	// transformation is invisible to the caller. On error, return 0 so
	// the caller can retry the original p without trying to reason about
	// partial writes of redacted text.
	if _, err := w.inner.Write([]byte(r.RedactEncoded(string(p)))); err != nil {
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

// reattachDebugFileSink wraps logger with a tee branch onto the active
// debug-file sink (registered via SetDebugFileSink), reusing the
// existing buffered + capped writer chain so any in-flight rotation
// state is preserved. No-op when no sink is registered.
//
// Called by every helper that REBUILDS the core from scratch
// (AddMode, ChangeLogLevel, RedirectToStderr, ChangeColorEncoding) —
// without it, those helpers silently discard the file-sink tee that
// AddDebugFileSink installed at boot, and subsequent debug records
// only land on stdout/stderr. That is the bug that caused the
// keploy-agent's per-test-set agent-debug.log files to come out
// empty even though the rotation primitive was firing correctly:
// the tee was already gone by the time the first BeforeSimulate
// rotation ran.
func reattachDebugFileSink(logger *zap.Logger) *zap.Logger {
	sink := GetDebugFileSink()
	if sink == nil || sink.buffered == nil {
		return logger
	}
	encoder := NewANSIConsoleEncoder(LogCfg.EncoderConfig)
	debugCore := newRedactingCore(zapcore.NewCore(
		encoder,
		wrapWriter(sink.buffered),
		zap.NewAtomicLevelAt(zap.DebugLevel),
	))
	return logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return zapcore.NewTee(c, debugCore)
	}))
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
	return reattachDebugFileSink(logger), nil
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
	return reattachDebugFileSink(logger), nil
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
	return reattachDebugFileSink(logger), nil
}

func ChangeColorEncoding() (*zap.Logger, error) {
	// For non-color mode, use the standard console encoder.
	LogCfg.Encoding = "console"
	LogCfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder

	// Build the core ourselves rather than using LogCfg.Build so the
	// underlying WriteSyncer passes through wrapWriter. zap.Config.Build
	// creates a stock ioCore whose sink we can't reach afterwards, which
	// would mean the post-encode RedactEncoded pass never runs in
	// --disable-ansi mode and non-string zap fields (Any/Binary/Reflect)
	// would leak from that path.
	encoder := zapcore.NewConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		wrapWriter(zapcore.AddSync(os.Stdout)),
		LogCfg.Level,
	)
	return reattachDebugFileSink(zap.New(newRedactingCore(core))), nil
}

// cappedWriteSyncer wraps a WriteSyncer and stops accepting bytes once
// the running total of accepted bytes reaches cap. Past the cap, Write
// reports success and discards the input — this is intentional so that
// hitting the cap never tears down the logger or causes the goroutine
// logging to error out. The caller queries Capped() at the end of the
// run to learn whether truncation occurred.
//
// The inner WriteSyncer (and its underlying file, if owned) can be
// swapped at runtime via Swap. Used for per-test-set log rotation in
// the keploy-agent: BeforeSimulate flips the file when the test set
// changes. Writes are serialized through s.mu so a concurrent Write
// can't tear into a half-swapped inner.
type cappedWriteSyncer struct {
	mu      sync.Mutex
	inner   zapcore.WriteSyncer
	cap     int64
	written atomic.Int64
	capped  atomic.Bool
}

func newCappedWriteSyncer(inner zapcore.WriteSyncer, cap int64) *cappedWriteSyncer {
	return &cappedWriteSyncer{inner: inner, cap: cap}
}

func (s *cappedWriteSyncer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	written := s.written.Load()
	if written >= s.cap {
		s.capped.Store(true)
		return len(p), nil
	}
	remaining := s.cap - written
	if int64(len(p)) > remaining {
		n, err := s.inner.Write(p[:remaining])
		s.written.Add(int64(n))
		s.capped.Store(true)
		// Report we accepted the full slice so zap doesn't retry; the
		// overflow is intentionally dropped.
		return len(p), err
	}
	n, err := s.inner.Write(p)
	s.written.Add(int64(n))
	return n, err
}

func (s *cappedWriteSyncer) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Sync()
}

func (s *cappedWriteSyncer) Capped() bool   { return s.capped.Load() }
func (s *cappedWriteSyncer) Written() int64 { return s.written.Load() }

// swap atomically replaces the inner WriteSyncer and resets the cap
// counters. Called by DebugFileSink.Swap after the upstream buffer has
// been flushed; bytes written before swap are guaranteed to land in
// the OLD inner because this method holds the write mutex.
func (s *cappedWriteSyncer) swap(inner zapcore.WriteSyncer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner = inner
	s.written.Store(0)
	s.capped.Store(false)
}

// DebugFileSink is the caller-side handle for the debug-level file sink
// attached by AddDebugFileSink. It owns the buffered + capped writer
// chain in front of the underlying file. Flush before closing the file
// to guarantee all in-flight bytes hit disk.
//
// The mu mutex serializes Swap against itself; concurrent Writes through
// the buffered/capped chain are still safe via cappedWriteSyncer.mu.
type DebugFileSink struct {
	mu           sync.Mutex
	capped       *cappedWriteSyncer
	buffered     *zapcore.BufferedWriteSyncer
	originPath   string // path the sink was originally bound to (best-effort, used to derive rotation paths)
	currentScope string // last scope value seen by RotateForScope (e.g. test-set ID); empty for the original file
}

// Flush flushes the in-memory write buffer to the underlying file.
// Call before closing the file at the end of a run.
func (s *DebugFileSink) Flush() error {
	if s == nil || s.buffered == nil {
		return nil
	}
	return s.buffered.Sync()
}

// Capped reports whether the sink dropped any bytes due to its cap.
// Call after Flush at end-of-run to populate bundle metadata.
func (s *DebugFileSink) Capped() bool {
	if s == nil || s.capped == nil {
		return false
	}
	return s.capped.Capped()
}

// Written reports how many bytes were successfully written to the file.
func (s *DebugFileSink) Written() int64 {
	if s == nil || s.capped == nil {
		return 0
	}
	return s.capped.Written()
}

// AddDebugFileSink returns a new logger that, in addition to whatever
// sinks the input logger already had, writes every debug-level-or-above
// entry to file via a 256 KiB buffered, capBytes-capped pipeline. Used
// by `keploy cloud replay` to capture the full debug stream for the
// support bundle without lifting the console level.
//
// The new sink is composed alongside the input logger's existing core
// via zapcore.NewTee. Each branch keeps its own level filter (the
// existing console core honors LogCfg.Level; the new debug-file core
// is locked at DebugLevel). The new branch is wrapped in its own
// redactingCore so field-level redaction runs before bytes hit the
// file, and the writer is wrapped in redactingWriter so post-encode
// redaction catches non-string fields rendered via reflection.
//
// Caller owns `file`. Call DebugFileSink.Flush before closing the file
// at end-of-run.
func AddDebugFileSink(logger *zap.Logger, file *os.File, capBytes int64) (*zap.Logger, *DebugFileSink) {
	if logger == nil || file == nil {
		return logger, nil
	}
	if capBytes <= 0 {
		capBytes = 100 << 20 // 100 MiB default
	}
	capped := newCappedWriteSyncer(zapcore.AddSync(file), capBytes)
	// FlushInterval defaults to 30 s in zap; that's too long for the
	// agent's per-test-set restart cadence in DockerCompose mode where
	// short-lived agent processes can shut down before a single auto-flush
	// fires, leaving the per-test-set scoped file empty. 500 ms is a
	// pragmatic compromise: records reach disk well before any plausible
	// SIGTERM grace window expires, with negligible overhead at the
	// volumes keploy actually emits.
	buffered := &zapcore.BufferedWriteSyncer{
		WS:            capped,
		Size:          256 << 10,
		FlushInterval: 500 * time.Millisecond,
	}
	encoder := NewANSIConsoleEncoder(LogCfg.EncoderConfig)
	debugCore := newRedactingCore(zapcore.NewCore(
		encoder,
		wrapWriter(buffered),
		zap.NewAtomicLevelAt(zap.DebugLevel),
	))
	newLogger := logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return zapcore.NewTee(c, debugCore)
	}))
	return newLogger, &DebugFileSink{
		capped:     capped,
		buffered:   buffered,
		originPath: file.Name(),
	}
}

// Swap atomically swaps the underlying file the sink writes to. The
// caller opens newFile; the previous file is returned so the caller
// can close it after Swap returns. Buffered writes are flushed to the
// previous file before the swap, and the cap-tripped state is reset.
//
// Concurrent log Writes are safe — they serialize through the inner
// cappedWriteSyncer's mutex, so no record can land half-written
// across the swap boundary.
func (s *DebugFileSink) Swap(newFile *os.File) error {
	if s == nil || s.capped == nil || s.buffered == nil || newFile == nil {
		return fmt.Errorf("debug file sink: swap called on uninitialized sink or nil file")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.buffered.Sync(); err != nil {
		return fmt.Errorf("debug file sink: flush before swap: %w", err)
	}
	s.capped.swap(zapcore.AddSync(newFile))
	s.originPath = newFile.Name()
	return nil
}

// RotateForScope swaps the sink to a per-scope file derived from the
// sink's origin path. Scope semantics are caller-defined; for the
// keploy-agent debug log this is the test set ID, and the resulting
// path is "<dir>/<scope>/<basename>" — e.g. an origin of
// "/keploy-host/agent-debug.log" with scope "test-set-3" yields
// "/keploy-host/test-set-3/agent-debug.log".
//
// On the first call with a given scope, the new directory is created,
// the new file is opened (truncated), the buffered writer is flushed
// to the previous file, the inner is swapped, and the previous file
// is closed. Repeated calls with the same scope are no-ops. An empty
// scope rotates back to the origin path.
//
// All errors are returned to the caller — typical caller logs at warn
// and proceeds without rotation rather than failing the surrounding
// operation. The method is concurrency-safe: parallel calls serialize
// through the sink mutex.
func (s *DebugFileSink) RotateForScope(scope string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if scope == s.currentScope {
		s.mu.Unlock()
		return nil
	}
	prevScope := s.currentScope
	prevOrigin := s.originPath
	s.mu.Unlock()

	// Compute the target path before reacquiring the mutex so we can
	// fail with no side effects if the origin path is unset.
	if prevOrigin == "" {
		return fmt.Errorf("debug file sink: origin path unset; cannot derive scoped path")
	}
	dir := filepath.Dir(prevOrigin)
	base := filepath.Base(prevOrigin)
	// Compute the absolute origin: when rotating away from a scoped
	// file back to the unscoped one, prevOrigin is the scoped path,
	// which sits one directory deeper than the original. Re-anchor by
	// chopping prevScope off prevOrigin's tail.
	originDir := dir
	if prevScope != "" && filepath.Base(dir) == prevScope {
		originDir = filepath.Dir(dir)
	}
	target := filepath.Join(originDir, scope, base)
	if scope == "" {
		target = filepath.Join(originDir, base)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("debug file sink: mkdir %q: %w", filepath.Dir(target), err)
	}
	newFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("debug file sink: open %q: %w", target, err)
	}

	s.mu.Lock()
	if err := s.buffered.Sync(); err != nil {
		s.mu.Unlock()
		_ = newFile.Close()
		return fmt.Errorf("debug file sink: flush before swap: %w", err)
	}
	s.capped.swap(zapcore.AddSync(newFile))
	s.originPath = target
	s.currentScope = scope
	s.mu.Unlock()

	// Write a synchronous header DIRECTLY to the new file, bypassing the
	// buffered chain, so the file is guaranteed non-empty even when the
	// process shuts down before the 500 ms flush interval fires or
	// before any further records are emitted post-rotation. Doubles as
	// an audit breadcrumb (operators can grep "rotated to scope" in the
	// bundle to confirm the rotation actually happened).
	header := fmt.Sprintf("[keploy log] rotated to scope=%q at %s\n",
		scope, time.Now().UTC().Format(time.RFC3339Nano))
	_, _ = newFile.WriteString(header)
	return nil
}

// CurrentScope reports the scope currently in effect (last value
// passed to RotateForScope), or "" if no rotation has happened.
func (s *DebugFileSink) CurrentScope() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentScope
}

// globalSinkHolder lets atomic.Value store the same concrete type for
// the package-wide debug file sink (atomic.Value panics on type
// changes across Stores).
type globalSinkHolder struct{ s *DebugFileSink }

var globalSink atomic.Value

// SetDebugFileSink registers s as the package-wide active debug file
// sink. Subsequent calls to DebugFileSink() return s. Pass nil to
// clear. Used by main entrypoints to publish the sink so cross-package
// helpers (e.g. RotateDebugFileForTestSet) can reach it without an
// explicit dependency injection chain.
func SetDebugFileSink(s *DebugFileSink) {
	globalSink.Store(globalSinkHolder{s: s})
}

// GetDebugFileSink returns the package-wide active sink registered by
// SetDebugFileSink, or nil when none is registered.
func GetDebugFileSink() *DebugFileSink {
	v := globalSink.Load()
	if v == nil {
		return nil
	}
	return v.(globalSinkHolder).s
}

// RotateDebugFileForTestSet is the package-level convenience helper
// the keploy-agent's BeforeSimulate route handler calls when a new
// test set begins. It locates the active sink (if any) and rotates
// to a per-test-set scope. Errors are returned for the caller to log;
// they are never fatal — log capture is a best-effort observability
// feature.
func RotateDebugFileForTestSet(testSetID string) error {
	sink := GetDebugFileSink()
	if sink == nil {
		return nil
	}
	return sink.RotateForScope(testSetID)
}
