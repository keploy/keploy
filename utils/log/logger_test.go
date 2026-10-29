package log

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
	encoder := newPlainConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(encoder, wrapWriter(w, encoder), LogCfg.Level)
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

// countingObject counts how often zap encodes it, so a test can tell an
// encoded-then-discarded record from one that was never encoded.
type countingObject struct{ n *int64 }

func (c countingObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	atomic.AddInt64(c.n, 1)
	enc.AddString("k", "v")
	return nil
}

// A full debug sink must not cost an encode per record. The file branch used to
// stay enabled past the cap, so every Debug call was formatted in full and then
// dropped by the capped writer: in a long replay that is most of the agent's
// logging, and all of it was wasted.
func TestAddDebugFileSink_CappedSinkSkipsEncoding(t *testing.T) {
	SetRedactor(nil)
	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel) // console never takes Debug

	tmp, err := os.CreateTemp(t.TempDir(), "capenc-*.log")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()
	wrapped, sink := AddDebugFileSink(logger, tmp, 2*1024)

	var encoded int64
	probe := zap.Object("probe", countingObject{&encoded})

	wrapped.Debug("before-cap", probe)
	if got := atomic.LoadInt64(&encoded); got != 1 {
		t.Fatalf("a Debug record below the cap was encoded %d times, want 1", got)
	}

	payload := strings.Repeat("x", 200)
	for i := 0; i < 50; i++ {
		wrapped.Debug(payload)
	}
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !sink.Capped() {
		t.Fatalf("precondition: sink should be capped after %d bytes", 50*len(payload))
	}

	atomic.StoreInt64(&encoded, 0)
	for i := 0; i < 10; i++ {
		wrapped.Debug("after-cap", probe)
	}
	if got := atomic.LoadInt64(&encoded); got != 0 {
		t.Errorf("a capped sink still encoded %d Debug records it can only discard", got)
	}

	// Info still reaches the console, which has its own level.
	wrapped.Info("info-after-cap", probe)
	if !strings.Contains(console.String(), "info-after-cap") {
		t.Errorf("capping the file sink silenced the console: %q", console.String())
	}

	// Swap re-arms the sink: records are encoded and land in the new file.
	next, err := os.CreateTemp(t.TempDir(), "capenc-next-*.log")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer next.Close()
	if err := sink.Swap(next); err != nil {
		t.Fatalf("swap: %v", err)
	}
	atomic.StoreInt64(&encoded, 0)
	wrapped.Debug("after-swap", probe)
	if got := atomic.LoadInt64(&encoded); got != 1 {
		t.Errorf("after Swap a Debug record was encoded %d times, want 1", got)
	}
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush after swap: %v", err)
	}
	contents, err := os.ReadFile(next.Name())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(contents), "after-swap") {
		t.Errorf("Debug record missing from the swapped-in file: %q", contents)
	}
}

// ObserveErrors is told each entry logged at ERROR or above once, also when
// the logger writes it to two of its cores (the console and the debug file),
// and nothing of a lower level; a nil observer stops it.
func TestObserveErrorsSeesEachErrorOnce(t *testing.T) {
	var got []string
	ObserveErrors(func(msg string) { got = append(got, msg) })
	t.Cleanup(func() { ObserveErrors(nil) })
	console, _ := observer.New(zapcore.DebugLevel)
	file, _ := observer.New(zapcore.DebugLevel)
	logger := zap.New(zapcore.NewTee(newRedactingCore(console), newRedactingCore(file)))
	logger.Warn("a warning")
	logger.Error("failed to load hooks")
	logger.Error("failed to start proxy")
	if len(got) != 2 || got[0] != "failed to load hooks" || got[1] != "failed to start proxy" {
		t.Fatalf("observed %q, want each ERROR once", got)
	}
	ObserveErrors(nil)
	logger.Error("after the observer stopped")
	if len(got) != 2 {
		t.Fatalf("observed %q after the observer stopped", got)
	}
}

// replacingRedactor replaces "hunter2" in encoded text.
type replacingRedactor struct{}

func (replacingRedactor) RedactEntry(*zapcore.Entry) {}
func (replacingRedactor) RedactField(*zapcore.Field) {}
func (replacingRedactor) RedactEncoded(text string) string {
	return strings.ReplaceAll(text, "hunter2", "[REDACTED]")
}

// Text that leaves the process outside a log line is redacted as a log line
// is, by the active redactor; with none it is as it was.
func TestRedactText(t *testing.T) {
	SetRedactor(nil)
	if got := RedactText("password hunter2"); got != "password hunter2" {
		t.Fatalf("with no redactor: %q", got)
	}
	SetRedactor(replacingRedactor{})
	t.Cleanup(func() { SetRedactor(nil) })
	if got := RedactText("password hunter2"); got != "password [REDACTED]" {
		t.Fatalf("with a redactor: %q", got)
	}
}

// cappedRunLog makes a file of the test's this run's log, held to capBytes
// (logFileRoom), with the console in another file of the test's, and gives
// back both and a logger writing them.
func cappedRunLog(t *testing.T, capBytes int64) (logger *zap.Logger, runLog, console *os.File) {
	t.Helper()
	dir := t.TempDir()
	console, err := os.Create(filepath.Join(dir, "console"))
	if err != nil {
		t.Fatal(err)
	}
	runLog, err = os.OpenFile(filepath.Join(dir, "run.log"), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	prevCap := logFileCap
	logFileCap = capBytes
	setPrimarySink(console)
	logFileSink.Store(runLog)
	t.Cleanup(func() {
		logFileSink.Store(nil)
		logFileCap = prevCap
		TestOnlyResetSink()
		_ = runLog.Close()
		_ = console.Close()
	})
	logger, err = ChangeLogLevel(zapcore.InfoLevel)
	if err != nil {
		t.Fatal(err)
	}
	return logger, runLog, console
}

func readAll(t *testing.T, f *os.File) string {
	t.Helper()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The run's log is often in memory (a tmpfs /tmp), and a --debug run logs
// without end, so the log holds at most its cap, as KEPLOY_DEBUG_FILE does:
// whole lines, the same the console got, for as long as one fits beside the
// closing note, then the note, once, and nothing more. The console still gets
// every line.
func TestTheRunsLogStopsAtItsCapWithOneNote(t *testing.T) {
	const capBytes = 4096
	logger, runLog, console := cappedRunLog(t, capBytes)
	pad := strings.Repeat("x", 60)
	for i := 0; i < 200; i++ {
		logger.Info(fmt.Sprintf("line %03d %s", i, pad))
	}
	got := readAll(t, runLog)
	said := readAll(t, console)
	if len(got) > capBytes {
		t.Errorf("the log holds %d bytes, past its cap of %d", len(got), capBytes)
	}
	if !strings.HasSuffix(got, logFileCapNote) || strings.Count(got, logFileCapNote) != 1 {
		t.Fatalf("the log does not end with the closing note, once; it holds:\n%s", got)
	}
	body := strings.TrimSuffix(got, logFileCapNote)
	if body == "" || !strings.HasSuffix(body, "\n") || !strings.HasPrefix(said, body) {
		t.Fatalf("the log before its note is not whole lines the console got first; it holds:\n%s", body)
	}
	next, _, _ := strings.Cut(said[len(body):], "\n")
	if len(body)+len(next)+1+len(logFileCapNote) <= capBytes {
		t.Errorf("the log stopped at %d bytes with room for the next line (%d bytes) and its note", len(body), len(next)+1)
	}
	for i := 0; i < 200; i++ {
		if !strings.Contains(said, fmt.Sprintf("line %03d %s\n", i, pad)) {
			t.Errorf("the console is missing line %03d", i)
		}
	}
	logger.Info("a line after the cap")
	if again := readAll(t, runLog); again != got {
		t.Errorf("a line after the cap changed the log:\n%s", again[len(got):])
	}
}

// The native agent appends to the CLI's log (ClientLogFileFlag), so the cap is
// the file's, not one process's: lines the other process wrote count. A log
// that is full when this process first writes it gets the note alone, and one
// the other process grows while this one is quiet holds no more than its cap.
func TestTheRunsLogCapCountsTheOtherProcesssLines(t *testing.T) {
	other := func(t *testing.T, runLog *os.File, n int) {
		t.Helper()
		f, err := os.OpenFile(runLog.Name(), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteString(strings.Repeat("a", n-1) + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("full before this process writes", func(t *testing.T) {
		const capBytes = 4096
		logger, runLog, _ := cappedRunLog(t, capBytes)
		other(t, runLog, capBytes-50)
		logger.Info("a CLI line")
		if got, want := readAll(t, runLog), strings.Repeat("a", capBytes-51)+"\n"+logFileCapNote; got != want {
			t.Errorf("the log holds:\n%s\nwant the other process's lines and the note alone", got)
		}
	})
	t.Run("grown while this process is quiet", func(t *testing.T) {
		const capBytes = 8192
		logger, runLog, _ := cappedRunLog(t, capBytes)
		pad := strings.Repeat("x", 60)
		for i := 0; i < 5; i++ {
			logger.Info(fmt.Sprintf("line %03d %s", i, pad))
		}
		other(t, runLog, 6000)
		for i := 5; i < 300; i++ {
			logger.Info(fmt.Sprintf("line %03d %s", i, pad))
		}
		got := readAll(t, runLog)
		if len(got) > capBytes {
			t.Errorf("the log holds %d bytes, past its cap of %d", len(got), capBytes)
		}
		if !strings.HasSuffix(got, logFileCapNote) || strings.Count(got, logFileCapNote) != 1 {
			t.Errorf("the log does not end with the closing note, once; it ends:\n%s", got[max(0, len(got)-300):])
		}
	})
}

// otherWriterEnv names the log TestLogFileWriterHelperProcess appends to, as
// the native agent appends to the CLI's, and otherWriterCapEnv its cap.
const (
	otherWriterEnv    = "KEPLOY_LOG_TEST_OTHER_WRITER"
	otherWriterCapEnv = "KEPLOY_LOG_TEST_OTHER_CAP"
)

// TestLogFileWriterHelperProcess is the other process of the rows below, a
// no-op in a plain test run. Started by startOtherProcess, it opens the log
// named by otherWriterEnv as the native agent opens the CLI's at its start
// (write-only, appending) and says "ready". Then, for each "LINES LAST" it
// reads, it writes LINES lines of 80 bytes there through a logFileWriter of
// its own, held to otherWriterCapEnv, then one of LAST bytes if LAST is not
// 0, and says "done".
func TestLogFileWriterHelperProcess(t *testing.T) {
	path := os.Getenv(otherWriterEnv)
	if path == "" {
		return
	}
	capBytes, err := strconv.ParseInt(os.Getenv(otherWriterCapEnv), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	logFileCap = capBytes
	logFileSink.Store(f)
	defer logFileSink.Store(nil)
	w := logFileWriter{}
	fmt.Println("ready")
	in := bufio.NewScanner(os.Stdin)
	n := 0
	for in.Scan() {
		var lines, last int
		if _, err := fmt.Sscan(in.Text(), &lines, &last); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < lines; i, n = i+1, n+1 {
			if _, err := fmt.Fprintf(w, "other %04d %s\n", n, strings.Repeat("o", 68)); err != nil {
				t.Fatal(err)
			}
		}
		if last > 0 {
			if _, err := w.Write([]byte(strings.Repeat("O", last-1) + "\n")); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Println("done")
	}
}

// otherProcess is a TestLogFileWriterHelperProcess, appending to a log as the
// native agent appends to its CLI's.
type otherProcess struct {
	in  io.WriteCloser
	out *bufio.Scanner
}

// startOtherProcess starts an otherProcess appending to runLog, with capBytes
// for the cap, and waits for it to have opened runLog. It ends with the test.
func startOtherProcess(t *testing.T, runLog *os.File, capBytes int64) *otherProcess {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLogFileWriterHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		otherWriterEnv+"="+runLog.Name(),
		otherWriterCapEnv+"="+strconv.FormatInt(capBytes, 10))
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Wait()
	})
	p := &otherProcess{in: in, out: bufio.NewScanner(out)}
	p.await(t, "ready")
	return p
}

// write has the other process write lines lines of 80 bytes, then one of last
// bytes if last is not 0, and waits for it to have.
func (p *otherProcess) write(t *testing.T, lines, last int) {
	t.Helper()
	if _, err := fmt.Fprintf(p.in, "%d %d\n", lines, last); err != nil {
		t.Fatal(err)
	}
	p.await(t, "done")
}

func (p *otherProcess) await(t *testing.T, word string) {
	t.Helper()
	var said []string
	for p.out.Scan() {
		if p.out.Text() == word {
			return
		}
		said = append(said, p.out.Text())
	}
	t.Fatalf("the other process ended before it said %q; it said:\n%s", word, strings.Join(said, "\n"))
}

// The CLI and its native agent write one log (ClientLogFileFlag), two
// processes each with a logFileWriter of its own, and either can fill it:
// while the other is quiet, with a last line too long to fit that leaves room
// for shorter ones, or in turns with the other. Whichever fills it, the log
// ends with the closing note, once, within its cap, sealed (its owner can
// read it and keploy writes it no more), and no line of either process
// follows the note.
func TestTheRunsLogEndsWithOneNoteWhicheverProcessFillsIt(t *testing.T) {
	const capBytes = 8192
	pad := strings.Repeat("x", 60)
	lines := func(logger *zap.Logger, from, to int) {
		for i := from; i < to; i++ {
			logger.Info(fmt.Sprintf("line %03d %s", i, pad))
		}
	}
	for _, c := range []struct {
		name string
		run  func(t *testing.T, logger *zap.Logger, other *otherProcess)
	}{
		{"the other process fills it while this one is quiet", func(t *testing.T, logger *zap.Logger, other *otherProcess) {
			lines(logger, 0, 3)
			other.write(t, 200, 0)
			lines(logger, 3, 50)
		}},
		{"the other process's last line is too long to fit", func(t *testing.T, logger *zap.Logger, other *otherProcess) {
			lines(logger, 0, 3)
			other.write(t, 10, capBytes)
			lines(logger, 3, 50)
			other.write(t, 10, 0)
		}},
		{"this process fills it, then the other writes", func(t *testing.T, logger *zap.Logger, other *otherProcess) {
			other.write(t, 1, 0)
			lines(logger, 0, 200)
			other.write(t, 50, 0)
		}},
		{"this process's last line is too long to fit, then the other writes", func(t *testing.T, logger *zap.Logger, other *otherProcess) {
			other.write(t, 1, 0)
			lines(logger, 0, 10)
			logger.Info(strings.Repeat("y", capBytes))
			other.write(t, 50, 0)
			lines(logger, 10, 20)
		}},
		{"both write past it, in turns", func(t *testing.T, logger *zap.Logger, other *otherProcess) {
			for round := 0; round < 5; round++ {
				lines(logger, round*30, round*30+30)
				other.write(t, 30, 0)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			logger, runLog, _ := cappedRunLog(t, capBytes)
			c.run(t, logger, startOtherProcess(t, runLog, capBytes))
			got := readAll(t, runLog)
			if len(got) > capBytes {
				t.Errorf("the log holds %d bytes, past its cap of %d", len(got), capBytes)
			}
			if !strings.HasSuffix(got, logFileCapNote) || strings.Count(got, logFileCapNote) != 1 {
				t.Errorf("the log does not end with the closing note, once (%d notes); it ends:\n%s",
					strings.Count(got, logFileCapNote), got[max(0, len(got)-400):])
			}
			if !strings.Contains(got, "line 000") || !strings.Contains(got, "other 0000") {
				t.Errorf("the log is missing the first line of one of its two processes:\n%s", got[:min(len(got), 400)])
			}
			fi, err := os.Stat(runLog.Name())
			if err != nil {
				t.Fatal(err)
			}
			// Sealed: its owner can read it and no keploy process writes it.
			// Bits, not the whole mode: Windows reports a read-only file as
			// 0444, where Unix has logFileSealedMode.
			if perm := fi.Mode().Perm(); perm&0o200 != 0 || perm&0o400 == 0 {
				t.Errorf("the full log's mode is %v, want its owner's write bit gone and read bit kept", perm)
			}
		})
	}
}
