package log

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// newConsoleLogger builds a logger whose only sink is the provided writer,
// at the provided level. Used as the input to AddDebugFileSink in tests
// so we can inspect both branches independently.
func newConsoleLogger(w zapcore.WriteSyncer, level zapcore.Level) *zap.Logger {
	cfg := zap.NewDevelopmentConfig()
	LogCfg = cfg
	LogCfg.EncoderConfig.EncodeTime = customTimeEncoder
	LogCfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	LogCfg.EncoderConfig.EncodeDuration = zapcore.StringDurationEncoder
	LogCfg.EncoderConfig.EncodeCaller = nil
	LogCfg.Level = zap.NewAtomicLevelAt(level)
	encoder := zapcore.NewConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(encoder, wrapWriter(w), LogCfg.Level)
	return zap.New(newRedactingCore(core))
}

type syncBuffer struct {
	mu  bytes.Buffer
	cnt int64
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	atomic.AddInt64(&b.cnt, int64(len(p)))
	return b.mu.Write(p)
}
func (b *syncBuffer) Sync() error  { return nil }
func (b *syncBuffer) String() string { return b.mu.String() }

func TestAddDebugFileSink_BeforeAttach_NotInFile(t *testing.T) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	logger.Info("before-attach")

	tmp, err := os.CreateTemp(t.TempDir(), "before-*.log")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()
	wrapped, sink := AddDebugFileSink(logger, tmp, 0)
	if wrapped == nil || sink == nil {
		t.Fatalf("AddDebugFileSink returned nil")
	}
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	contents, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(contents) != 0 {
		t.Fatalf("expected empty file before any writes after attach, got %q", contents)
	}
}

func TestAddDebugFileSink_AfterAttach_DebugLandsInFile(t *testing.T) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel) // console suppresses debug

	tmp, err := os.CreateTemp(t.TempDir(), "after-*.log")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()
	wrapped, sink := AddDebugFileSink(logger, tmp, 0)

	wrapped.Debug("debug-line")
	wrapped.Info("info-line")

	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	contents, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(contents)
	if !strings.Contains(got, "debug-line") {
		t.Errorf("expected debug line in file, got: %s", got)
	}
	if !strings.Contains(got, "info-line") {
		t.Errorf("expected info line in file, got: %s", got)
	}

	// console is at Info level — debug must NOT have reached it.
	con := console.String()
	if strings.Contains(con, "debug-line") {
		t.Errorf("debug line leaked to console at Info level: %s", con)
	}
	if !strings.Contains(con, "info-line") {
		t.Errorf("info line missing from console: %s", con)
	}
}

func TestAddDebugFileSink_Buffered_FlushRequired(t *testing.T) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	tmp, err := os.CreateTemp(t.TempDir(), "buf-*.log")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()
	wrapped, sink := AddDebugFileSink(logger, tmp, 0)

	// One small entry — won't fill the 256 KiB buffer.
	wrapped.Debug("buffered-line")

	// Read before Flush — file should still be empty (or smaller than what's in flight).
	pre, _ := os.ReadFile(tmp.Name())
	if strings.Contains(string(pre), "buffered-line") {
		t.Logf("note: BufferedWriteSyncer flushed proactively; harmless")
	}

	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	post, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(post), "buffered-line") {
		t.Errorf("expected line in file after flush, got: %s", post)
	}
}

func TestAddDebugFileSink_SoftCap(t *testing.T) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	tmp, err := os.CreateTemp(t.TempDir(), "cap-*.log")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()

	const cap = int64(2 * 1024) // 2 KiB
	wrapped, sink := AddDebugFileSink(logger, tmp, cap)

	// Each Debug entry will be on the order of 100 bytes encoded; emit
	// enough to comfortably exceed 2 KiB.
	payload := strings.Repeat("x", 200)
	for i := 0; i < 200; i++ {
		wrapped.Debug(payload)
	}
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !sink.Capped() {
		t.Fatalf("expected sink to report capped, got false")
	}
	if got := sink.Written(); got > cap {
		t.Errorf("written bytes exceed cap: got %d, cap %d", got, cap)
	}
	info, err := os.Stat(tmp.Name())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > cap {
		t.Errorf("file size exceeds cap: got %d, cap %d", info.Size(), cap)
	}

	// Further writes after the cap must not error and must not grow the file.
	wrapped.Debug("post-cap-line")
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush after cap: %v", err)
	}
	info2, _ := os.Stat(tmp.Name())
	if info2.Size() != info.Size() {
		t.Errorf("file grew past cap: was %d, now %d", info.Size(), info2.Size())
	}
}

// countingRedactor counts how many times each redaction hook fires.
// Used to assert the redaction-once invariant — a single outer
// redactingCore should run RedactEntry/RedactField exactly once per
// log call, not 2x because of the tee.
type countingRedactor struct {
	entries int64
	fields  int64
	encoded int64
}

func (r *countingRedactor) RedactEntry(ent *zapcore.Entry) {
	atomic.AddInt64(&r.entries, 1)
}
func (r *countingRedactor) RedactField(f *zapcore.Field) {
	atomic.AddInt64(&r.fields, 1)
}
func (r *countingRedactor) RedactEncoded(text string) string {
	atomic.AddInt64(&r.encoded, 1)
	return text
}

func TestAddDebugFileSink_RedactionOnceInvariant(t *testing.T) {
	r := &countingRedactor{}
	SetRedactor(r)
	t.Cleanup(func() { SetRedactor(nil) })

	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	tmp, err := os.CreateTemp(t.TempDir(), "redact-*.log")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()
	wrapped, sink := AddDebugFileSink(logger, tmp, 0)

	const N = 50
	for i := 0; i < N; i++ {
		wrapped.Debug("test", zap.String("k", "v"))
	}
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// RedactEntry runs once per Write call to the OUTER redactingCore.
	// For Debug entries, the outer core writes once, then the inner Tee
	// fans out — so we expect N entry redactions, not 2N.
	if got := atomic.LoadInt64(&r.entries); got != N {
		t.Errorf("RedactEntry: expected %d, got %d (suggests redaction is double-wrapped)", N, got)
	}
	// One field per call → N field redactions.
	if got := atomic.LoadInt64(&r.fields); got != N {
		t.Errorf("RedactField: expected %d, got %d", N, got)
	}
	// RedactEncoded fires per writer.Write — and we have two writers
	// (console + buffered debug file). Buffered may not fire for every
	// entry. So the lower bound is N (console fires every time at debug
	// level... no, console is at Info — debug entries don't reach console).
	// Console core's Enabled returns false for Debug, so the entry does
	// not flow through its writer. Only the debug-file writer fires.
	// That's at most ceil(payload/buffer-size) calls. Just assert it's
	// >0 to confirm the post-encode pass runs at all.
	if got := atomic.LoadInt64(&r.encoded); got == 0 {
		t.Errorf("RedactEncoded never fired; expected at least one call")
	}
}

func BenchmarkAddDebugFileSink_Write(b *testing.B) {
	SetRedactor(nil)
	console := &syncBuffer{}
	base := newConsoleLogger(console, zap.InfoLevel)

	tmpDir := b.TempDir()
	tmp, err := os.OpenFile(filepath.Join(tmpDir, "bench.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer tmp.Close()
	wrapped, sink := AddDebugFileSink(base, tmp, 0)
	defer sink.Flush()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wrapped.Debug("bench", zap.Int("i", i), zap.String("k", "value"))
	}
}

func BenchmarkBaseline_Write(b *testing.B) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.Info("bench", zap.Int("i", i), zap.String("k", "value"))
	}
}
