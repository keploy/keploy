package log

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
func (b *syncBuffer) Sync() error    { return nil }
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

func TestDebugFileSink_RotateForScope(t *testing.T) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	dir := t.TempDir()
	originPath := filepath.Join(dir, "agent-debug.log")
	originFile, err := os.Create(originPath)
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	defer originFile.Close()

	wrapped, sink := AddDebugFileSink(logger, originFile, 0)
	if sink == nil {
		t.Fatalf("AddDebugFileSink returned nil")
	}
	defer SetDebugFileSink(nil)

	wrapped.Debug("origin-line")

	if err := sink.RotateForScope("test-set-1"); err != nil {
		t.Fatalf("RotateForScope: %v", err)
	}
	wrapped.Debug("scope-1-line")

	if err := sink.RotateForScope("test-set-2"); err != nil {
		t.Fatalf("RotateForScope: %v", err)
	}
	wrapped.Debug("scope-2-line")

	// Repeat scope is a no-op.
	if err := sink.RotateForScope("test-set-2"); err != nil {
		t.Fatalf("repeat RotateForScope: %v", err)
	}
	wrapped.Debug("scope-2-line-2")

	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	originBytes, _ := os.ReadFile(originPath)
	if !strings.Contains(string(originBytes), "origin-line") {
		t.Errorf("origin-line missing from %s: %s", originPath, originBytes)
	}
	if strings.Contains(string(originBytes), "scope-1-line") {
		t.Errorf("scope-1-line leaked into origin file: %s", originBytes)
	}

	scope1Path := filepath.Join(dir, "test-set-1", "agent-debug.log")
	scope1Bytes, err := os.ReadFile(scope1Path)
	if err != nil {
		t.Fatalf("read scope-1 file: %v", err)
	}
	if !strings.Contains(string(scope1Bytes), "scope-1-line") {
		t.Errorf("scope-1-line missing from %s: %s", scope1Path, scope1Bytes)
	}
	if strings.Contains(string(scope1Bytes), "scope-2-line") {
		t.Errorf("scope-2-line leaked into scope-1 file: %s", scope1Bytes)
	}

	scope2Path := filepath.Join(dir, "test-set-2", "agent-debug.log")
	scope2Bytes, err := os.ReadFile(scope2Path)
	if err != nil {
		t.Fatalf("read scope-2 file: %v", err)
	}
	if !strings.Contains(string(scope2Bytes), "scope-2-line") || !strings.Contains(string(scope2Bytes), "scope-2-line-2") {
		t.Errorf("scope-2 file missing expected lines: %s", scope2Bytes)
	}

	if got := sink.CurrentScope(); got != "test-set-2" {
		t.Errorf("CurrentScope: got %q, want test-set-2", got)
	}
}

func TestDebugFileSink_RotateBackToOrigin(t *testing.T) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	dir := t.TempDir()
	originPath := filepath.Join(dir, "agent-debug.log")
	originFile, err := os.Create(originPath)
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	defer originFile.Close()

	wrapped, sink := AddDebugFileSink(logger, originFile, 0)
	defer SetDebugFileSink(nil)

	if err := sink.RotateForScope("ts-a"); err != nil {
		t.Fatalf("RotateForScope: %v", err)
	}
	wrapped.Debug("scoped-record")

	if err := sink.RotateForScope(""); err != nil {
		t.Fatalf("RotateForScope back to origin: %v", err)
	}
	wrapped.Debug("post-rotation-origin-record")

	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	originBytes, _ := os.ReadFile(originPath)
	if !strings.Contains(string(originBytes), "post-rotation-origin-record") {
		t.Errorf("post-rotation record missing from origin: %s", originBytes)
	}
	scopedPath := filepath.Join(dir, "ts-a", "agent-debug.log")
	scopedBytes, _ := os.ReadFile(scopedPath)
	if !strings.Contains(string(scopedBytes), "scoped-record") {
		t.Errorf("scoped record missing from scoped file: %s", scopedBytes)
	}
}

// TestDebugFileSink_RotateForScope_RejectsPathTraversal is the
// regression test for the path traversal in RotateForScope. The scope
// is the test-set ID, which reaches the agent straight off the wire
// (HandleBeforeTestSetCompose json-decodes it out of an HTTP request
// body), so it is fully caller-controlled. The rotation target used to
// be filepath.Join'd from it with no validation, and the file is
// opened with O_TRUNC — so a scope of "../<name>" truncated an
// arbitrary file the process could write, which for keploy is often
// everything because the eBPF hooks need root.
func TestDebugFileSink_RotateForScope_RejectsPathTraversal(t *testing.T) {
	SetRedactor(nil)

	root := t.TempDir()
	originDir := filepath.Join(root, "origin")
	if err := os.MkdirAll(originDir, 0o755); err != nil {
		t.Fatalf("mkdir origin dir: %v", err)
	}

	// Canary outside the origin directory: scope "../escaped" resolves
	// to exactly this file, and the O_TRUNC open empties it.
	victimDir := filepath.Join(root, "escaped")
	if err := os.MkdirAll(victimDir, 0o755); err != nil {
		t.Fatalf("mkdir victim dir: %v", err)
	}
	victimPath := filepath.Join(victimDir, "agent-debug.log")
	const victimContents = "precious-contents"
	if err := os.WriteFile(victimPath, []byte(victimContents), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)
	originPath := filepath.Join(originDir, "agent-debug.log")
	originFile, err := os.Create(originPath)
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	defer originFile.Close()

	wrapped, sink := AddDebugFileSink(logger, originFile, 0)
	if sink == nil {
		t.Fatalf("AddDebugFileSink returned nil")
	}
	defer SetDebugFileSink(nil)
	wrapped.Debug("origin-line")

	// Every one of these must be refused before any filesystem call.
	// The windows-flavoured ones ("..\\escaped", "C:") are inert on
	// linux/darwin but must be rejected on every platform so the linux
	// build refuses exactly what the windows runtime would honour.
	for _, scope := range []string{
		"../escaped",
		"..",
		".",
		"nested/child",
		`..\escaped`,
		filepath.Join(root, "absolute-escape"),
		"C:",
	} {
		err := sink.RotateForScope(scope)
		if err == nil {
			t.Errorf("RotateForScope(%q): expected rejection, got nil", scope)
			continue
		}
		// The rejection must name the offending value; errors quote it
		// with %q, so compare against the quoted form.
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", scope)) {
			t.Errorf("RotateForScope(%q): error must name the offending scope, got %v", scope, err)
		}
		if got := sink.CurrentScope(); got != "" {
			t.Errorf("RotateForScope(%q) was accepted: CurrentScope=%q", scope, got)
		}
	}

	if got, err := os.ReadFile(victimPath); err != nil || string(got) != victimContents {
		t.Errorf("file outside the origin dir was truncated/rewritten: contents=%q err=%v", got, err)
	}

	// Nothing may have been created next to the origin directory either
	// (scope ".." targets <root>/agent-debug.log).
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "origin" && e.Name() != "escaped" {
			t.Errorf("RotateForScope created %q outside the origin directory", filepath.Join(root, e.Name()))
		}
	}

	// A rejected rotation must leave the sink usable and still bound to
	// the previous file rather than half-swapped.
	wrapped.Debug("still-on-origin")
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	originBytes, _ := os.ReadFile(originPath)
	if !strings.Contains(string(originBytes), "origin-line") || !strings.Contains(string(originBytes), "still-on-origin") {
		t.Errorf("origin file lost records across the rejected rotations: %s", originBytes)
	}

	// A legitimate single-segment scope still rotates.
	if err := sink.RotateForScope("test-set-0"); err != nil {
		t.Fatalf("RotateForScope on a valid scope: %v", err)
	}
	wrapped.Debug("scoped-line")
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	scopedBytes, err := os.ReadFile(filepath.Join(originDir, "test-set-0", "agent-debug.log"))
	if err != nil {
		t.Fatalf("read scoped file: %v", err)
	}
	if !strings.Contains(string(scopedBytes), "scoped-line") {
		t.Errorf("scoped-line missing from scoped file: %s", scopedBytes)
	}
}

func TestValidateScope(t *testing.T) {
	valid := []string{
		"",            // rotate back to origin
		"test-set-0",  // the real-world shape
		"v1..v2",      // ".." as a substring, not as a path element
		"..hidden",    //
		"a.b.c",       //
		"UPPER_case1", //
	}
	for _, scope := range valid {
		if err := validateScope(scope); err != nil {
			t.Errorf("validateScope(%q): want nil, got %v", scope, err)
		}
	}

	invalid := []string{
		".", "..", "../x", "a/b", "/abs", `a\b`, `..\x`, `\\server\share`,
		"C:", "C:/x", "./a", "a/", "x/..",
	}
	for _, scope := range invalid {
		if err := validateScope(scope); err == nil {
			t.Errorf("validateScope(%q): want rejection, got nil", scope)
		}
	}
}

// TestDebugFileSink_RotateForScope_RejectsSymlinkEscape covers what a purely
// lexical containment check (Abs + Clean + Rel) cannot: the scope is a
// perfectly valid single segment, but a symlink already sitting in the debug
// log directory points out of it. A plain MkdirAll + O_TRUNC open follows that
// link and truncates the file on the other side — with the agent running as
// root for its eBPF hooks, and the debug log directory being the user's
// git-committed keploy folder bind-mounted into the container, both halves of
// that setup are attacker/repo-reachable.
func TestDebugFileSink_RotateForScope_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on windows")
	}
	SetRedactor(nil)

	root := t.TempDir()
	originDir := filepath.Join(root, "origin")
	outsideDir := filepath.Join(root, "outside")
	for _, d := range []string{originDir, outsideDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	const victimContents = "precious-contents"
	// Victim 1: reached through a symlinked SCOPE DIRECTORY.
	victimViaDir := filepath.Join(outsideDir, "agent-debug.log")
	if err := os.WriteFile(victimViaDir, []byte(victimContents), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(originDir, "dir-link")); err != nil {
		t.Fatalf("symlink scope dir: %v", err)
	}
	// Victim 2: reached through a symlinked TARGET FILE inside a real dir.
	victimViaFile := filepath.Join(root, "shadow")
	if err := os.WriteFile(victimViaFile, []byte(victimContents), 0o644); err != nil {
		t.Fatalf("write shadow: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(originDir, "file-link"), 0o755); err != nil {
		t.Fatalf("mkdir file-link: %v", err)
	}
	if err := os.Symlink(victimViaFile, filepath.Join(originDir, "file-link", "agent-debug.log")); err != nil {
		t.Fatalf("symlink target file: %v", err)
	}

	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)
	originPath := filepath.Join(originDir, "agent-debug.log")
	originFile, err := os.Create(originPath)
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	defer originFile.Close()

	wrapped, sink := AddDebugFileSink(logger, originFile, 0)
	if sink == nil {
		t.Fatalf("AddDebugFileSink returned nil")
	}
	defer SetDebugFileSink(nil)

	for _, scope := range []string{"dir-link", "file-link"} {
		if err := sink.RotateForScope(scope); err == nil {
			t.Errorf("RotateForScope(%q): expected rejection of a symlinked target, got nil", scope)
		}
		if got := sink.CurrentScope(); got != "" {
			t.Errorf("RotateForScope(%q) was accepted: CurrentScope=%q", scope, got)
		}
	}

	// Writes must still land on the origin file, and neither victim may have
	// been touched.
	wrapped.Debug("still-on-origin")
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	for _, victim := range []string{victimViaDir, victimViaFile} {
		got, err := os.ReadFile(victim)
		if err != nil || string(got) != victimContents {
			t.Errorf("file outside the debug log dir was truncated through a symlink: %s contents=%q err=%v", victim, got, err)
		}
	}
	originBytes, _ := os.ReadFile(originPath)
	if !strings.Contains(string(originBytes), "still-on-origin") {
		t.Errorf("sink stopped writing to the origin file after the rejections: %s", originBytes)
	}
}

// TestDebugFileSink_RotateForScope_ClosesPreviousFile pins the descriptor
// lifetime the RotateForScope doc promises. The scope is caller-supplied and
// unbounded in cardinality, so leaking one descriptor per rotation (until the
// GC happens to run os.File's finalizer) walks the agent's descriptor table up
// under a burst of distinct test sets.
//
// The file the CALLER supplied to AddDebugFileSink is explicitly excluded: the
// caller owns it and closes it itself.
func TestDebugFileSink_RotateForScope_ClosesPreviousFile(t *testing.T) {
	SetRedactor(nil)

	dir := t.TempDir()
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)
	originPath := filepath.Join(dir, "agent-debug.log")
	originFile, err := os.Create(originPath)
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	defer originFile.Close()

	wrapped, sink := AddDebugFileSink(logger, originFile, 0)
	if sink == nil {
		t.Fatalf("AddDebugFileSink returned nil")
	}
	defer SetDebugFileSink(nil)

	const rotations = 40
	for i := 0; i < rotations; i++ {
		if err := sink.RotateForScope(fmt.Sprintf("ts-%d", i)); err != nil {
			t.Fatalf("RotateForScope(ts-%d): %v", i, err)
		}
		wrapped.Debug(fmt.Sprintf("line-%d", i))
	}
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// The caller's file must still be usable — the sink must not have closed
	// it out from under the caller on the first rotation.
	if _, err := originFile.WriteString("caller-still-owns-this\n"); err != nil {
		t.Errorf("sink closed the caller-owned origin file: %v", err)
	}

	// Every rotated-away file must be closed. os.File.Close on an
	// already-closed file reports ErrClosed, which is what we assert against
	// the sink's own bookkeeping: only the CURRENT file may still be open.
	sink.mu.Lock()
	current := sink.owned
	sink.mu.Unlock()
	if current == nil {
		t.Fatal("sink did not record ownership of the rotated file")
	}
	if _, err := current.WriteString(""); err != nil {
		t.Errorf("the current rotated file must stay open: %v", err)
	}
	// At most two descriptors under dir may remain: the caller-owned origin
	// file and the file the last rotation opened. A leak shows up as ~one per
	// rotation. (Linux-only introspection; a no-op elsewhere.)
	if n := countOpenFDs(t, dir); n > 2 {
		t.Errorf("rotation leaked descriptors: %d files under %s still open after %d rotations, want ≤2", n, dir, rotations)
	}
}

// countOpenFDs reports how many of this process's open descriptors point at a
// file under dir. Linux-only introspection; other platforms return 0 so the
// caller's assertion is a no-op there.
func countOpenFDs(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0 // not linux (or /proc not mounted) — nothing to assert
	}
	n := 0
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(target, dir+string(filepath.Separator)) {
			n++
		}
	}
	return n
}

func TestRotateDebugFileForTestSet_NilSinkIsSafe(t *testing.T) {
	SetDebugFileSink(nil)
	if err := RotateDebugFileForTestSet("anything"); err != nil {
		t.Errorf("RotateDebugFileForTestSet with nil sink: got %v, want nil", err)
	}
}

// TestDebugFileSink_SurvivesAddMode is the regression test for the
// empty-per-test-set-file bug: AddMode rebuilds the core from scratch
// and the file-sink tee was being silently discarded. Without
// reattachDebugFileSink in AddMode, the second log record would not
// appear in the file.
func TestDebugFileSink_SurvivesAddMode(t *testing.T) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	tmp, err := os.CreateTemp(t.TempDir(), "addmode-*.log")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()
	wrapped, sink := AddDebugFileSink(logger, tmp, 0)
	SetDebugFileSink(sink)
	defer SetDebugFileSink(nil)

	wrapped.Debug("before-addmode")

	// Simulate the agent CLI's Validate path: AddMode mutates the live
	// logger via *logger = *new (the same pointee-overwrite pattern
	// the codebase uses everywhere).
	rebuilt, err := AddMode("agent")
	if err != nil {
		t.Fatalf("AddMode: %v", err)
	}
	*wrapped = *rebuilt

	wrapped.Debug("after-addmode")

	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	contents, _ := os.ReadFile(tmp.Name())
	got := string(contents)
	if !strings.Contains(got, "before-addmode") {
		t.Errorf("before-addmode missing from file: %s", got)
	}
	if !strings.Contains(got, "after-addmode") {
		t.Errorf("after-addmode missing from file (the regression): %s", got)
	}
}

// TestDebugFileSink_RotateAfterAddMode confirms that rotation still
// works after the logger has been rebuilt — the buffered+capped
// chain is the same instance, so RotateForScope swaps a file that
// the post-AddMode tee branch is already writing to.
func TestDebugFileSink_RotateAfterAddMode(t *testing.T) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	dir := t.TempDir()
	originPath := filepath.Join(dir, "agent-debug.log")
	originFile, err := os.Create(originPath)
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	defer originFile.Close()

	wrapped, sink := AddDebugFileSink(logger, originFile, 0)
	SetDebugFileSink(sink)
	defer SetDebugFileSink(nil)

	rebuilt, _ := AddMode("agent")
	*wrapped = *rebuilt

	if err := sink.RotateForScope("ts-x"); err != nil {
		t.Fatalf("RotateForScope: %v", err)
	}
	wrapped.Debug("post-rebuild-post-rotate")

	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	scopedPath := filepath.Join(dir, "ts-x", "agent-debug.log")
	contents, err := os.ReadFile(scopedPath)
	if err != nil {
		t.Fatalf("read scoped file: %v", err)
	}
	if !strings.Contains(string(contents), "post-rebuild-post-rotate") {
		t.Errorf("post-rebuild-post-rotate missing from %s: %s", scopedPath, contents)
	}
}

func TestDebugFileSink_SurvivesChangeLogLevel(t *testing.T) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	tmp, _ := os.CreateTemp(t.TempDir(), "level-*.log")
	defer tmp.Close()
	wrapped, sink := AddDebugFileSink(logger, tmp, 0)
	SetDebugFileSink(sink)
	defer SetDebugFileSink(nil)

	rebuilt, err := ChangeLogLevel(zap.DebugLevel)
	if err != nil {
		t.Fatalf("ChangeLogLevel: %v", err)
	}
	*wrapped = *rebuilt

	wrapped.Debug("post-changeloglevel")
	_ = sink.Flush()
	contents, _ := os.ReadFile(tmp.Name())
	if !strings.Contains(string(contents), "post-changeloglevel") {
		t.Errorf("ChangeLogLevel dropped the file sink: %s", contents)
	}
}

func TestSetGetDebugFileSink(t *testing.T) {
	SetRedactor(nil)
	defer SetDebugFileSink(nil)

	if got := GetDebugFileSink(); got != nil {
		t.Errorf("GetDebugFileSink with nothing registered: got %v, want nil", got)
	}

	logger := newConsoleLogger(&syncBuffer{}, zap.InfoLevel)
	tmp, _ := os.CreateTemp(t.TempDir(), "global-*.log")
	defer tmp.Close()
	_, sink := AddDebugFileSink(logger, tmp, 0)
	SetDebugFileSink(sink)

	if got := GetDebugFileSink(); got != sink {
		t.Errorf("GetDebugFileSink: got %v, want %v", got, sink)
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
