package log

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestDebugFileSink_ProductionLikeRotation mimics the keploy-agent's
// real lifecycle: AddDebugFileSink at boot, AddMode swap during
// Validate, then concurrent goroutines emit records WHILE rotation
// fires between test sets. Asserts each scoped file ends up non-empty.
//
// Repro target: the bundle had per-test-set files of 0 bytes despite
// the rotation primitive firing.
func TestDebugFileSink_ProductionLikeRotation(t *testing.T) {
	SetRedactor(nil)
	defer SetDebugFileSink(nil)

	dir := t.TempDir()
	originPath := filepath.Join(dir, "agent-debug.log")
	originFile, err := os.Create(originPath)
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	defer originFile.Close()

	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	// Step 1: AddDebugFileSink at boot.
	wrapped, sink := AddDebugFileSink(logger, originFile, 0)
	*logger = *wrapped
	SetDebugFileSink(sink)

	// Step 2: emit boot-time records (these should land in origin file).
	for i := 0; i < 50; i++ {
		logger.Debug("boot-record", zap.Int("i", i))
	}

	// Step 3: simulate the agent CLI's Validate: AddMode("agent") which
	// rebuilds the core and (with the fix) re-attaches the file sink.
	rebuilt, err := AddMode("agent")
	if err != nil {
		t.Fatalf("AddMode: %v", err)
	}
	*logger = *rebuilt

	// Step 4: Spawn proxy-like goroutines that emit records.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
					logger.Debug("traffic-record",
						zap.Int("goroutine", gid),
						zap.Int("seq", i),
						zap.String("payload", "Handling outgoing connection to destination port"))
					i++
					time.Sleep(time.Millisecond) // pace the emissions
				}
			}
		}(g)
	}

	// Step 5: rotate through three test sets, with brief pauses to let
	// goroutines emit records into the active scope.
	scopes := []string{"test-set-a", "test-set-b", "test-set-c"}
	for _, scope := range scopes {
		if err := sink.RotateForScope(scope); err != nil {
			t.Fatalf("RotateForScope(%q): %v", scope, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	close(stop)
	wg.Wait()

	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Origin file should have boot records + the records emitted between
	// AddMode and the first rotation (a small window).
	if info, err := os.Stat(originPath); err != nil {
		t.Fatalf("stat origin: %v", err)
	} else if info.Size() == 0 {
		t.Errorf("origin file empty (expected boot records)")
	} else {
		t.Logf("origin file: %d bytes", info.Size())
	}

	// Each scoped file (except possibly the last, which had no Sync at
	// next-rotation, only the deferred Flush) should have content.
	for i, scope := range scopes {
		scopedPath := filepath.Join(dir, scope, "agent-debug.log")
		info, err := os.Stat(scopedPath)
		if err != nil {
			t.Errorf("[%d] stat %s: %v", i, scopedPath, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("[%d] scope %q file is 0 bytes (the bug)", i, scope)
		} else {
			t.Logf("[%d] scope %q file: %d bytes", i, scope, info.Size())
		}
	}
}

// TestDebugFileSink_ScopedFileNonEmptyEvenWithoutPostRotationWrites
// is the regression test for the empty-per-test-set-file production
// bug: in DockerCompose mode the agent restarts per test set, so a
// short-lived agent that fires a single BeforeSimulate (rotation)
// and then exits before its 500 ms buffer flush leaves the scoped
// file at 0 bytes. The fix writes a synchronous marker directly to
// the new file inside RotateForScope so the file is non-empty
// regardless of what the buffered chain does next.
func TestDebugFileSink_ScopedFileNonEmptyEvenWithoutPostRotationWrites(t *testing.T) {
	SetRedactor(nil)
	defer SetDebugFileSink(nil)

	dir := t.TempDir()
	originFile, err := os.Create(filepath.Join(dir, "agent-debug.log"))
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	defer originFile.Close()

	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)
	wrapped, sink := AddDebugFileSink(logger, originFile, 0)
	*logger = *wrapped
	SetDebugFileSink(sink)

	// Rotate, then immediately stop. NO records emitted post-rotation.
	if err := sink.RotateForScope("ts-empty"); err != nil {
		t.Fatalf("RotateForScope: %v", err)
	}
	// Deliberately skip Flush to model the SIGKILL / abrupt-shutdown case.

	scopedPath := filepath.Join(dir, "ts-empty", "agent-debug.log")
	contents, err := os.ReadFile(scopedPath)
	if err != nil {
		t.Fatalf("read %s: %v", scopedPath, err)
	}
	if len(contents) == 0 {
		t.Fatalf("scoped file empty even though rotation fired (the bug)")
	}
	if !strings.Contains(string(contents), "rotated to scope=") {
		t.Errorf("scoped file missing rotation marker: %s", contents)
	}
	if !strings.Contains(string(contents), `"ts-empty"`) {
		t.Errorf("rotation marker missing test-set ID: %s", contents)
	}
}

// TestDebugFileSink_RotationAfterFewWrites checks the specific
// edge case where only a small number of records are emitted between
// rotations — does the buffer auto-flush, or do those records sit in
// memory and get lost?
func TestDebugFileSink_RotationAfterFewWrites(t *testing.T) {
	SetRedactor(nil)
	defer SetDebugFileSink(nil)

	dir := t.TempDir()
	originPath := filepath.Join(dir, "agent-debug.log")
	originFile, err := os.Create(originPath)
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	defer originFile.Close()

	console := &syncBuffer{}
	logger := newConsoleLogger(console, zap.InfoLevel)

	wrapped, sink := AddDebugFileSink(logger, originFile, 0)
	*logger = *wrapped
	SetDebugFileSink(sink)

	rebuilt, _ := AddMode("agent")
	*logger = *rebuilt

	// Rotate to scope A; emit a few records; rotate to B.
	if err := sink.RotateForScope("scope-a"); err != nil {
		t.Fatalf("rotate to a: %v", err)
	}
	for i := 0; i < 5; i++ {
		logger.Debug("record-in-a", zap.Int("i", i),
			zap.String("payload", fmt.Sprintf("entry-%d", i)))
	}
	if err := sink.RotateForScope("scope-b"); err != nil {
		t.Fatalf("rotate to b: %v", err)
	}
	for i := 0; i < 5; i++ {
		logger.Debug("record-in-b", zap.Int("i", i))
	}

	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	scopeAPath := filepath.Join(dir, "scope-a", "agent-debug.log")
	if info, err := os.Stat(scopeAPath); err != nil {
		t.Errorf("stat scope-a: %v", err)
	} else if info.Size() == 0 {
		t.Errorf("scope-a file is 0 bytes — buffer didn't drain at rotation to scope-b (THIS IS THE PRODUCTION BUG)")
	} else {
		t.Logf("scope-a file: %d bytes", info.Size())
	}

	scopeBPath := filepath.Join(dir, "scope-b", "agent-debug.log")
	if info, err := os.Stat(scopeBPath); err != nil {
		t.Errorf("stat scope-b: %v", err)
	} else if info.Size() == 0 {
		t.Errorf("scope-b file is 0 bytes")
	} else {
		t.Logf("scope-b file: %d bytes", info.Size())
	}
}
