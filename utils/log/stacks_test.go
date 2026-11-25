package log

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The stacks go to a fresh file beside the run's log, its owner's alone, and
// hold every goroutine's stack: this test's, and one parked elsewhere.
func TestWriteStacksWritesEveryGoroutinesStackBesideTheLog(t *testing.T) {
	dir := t.TempDir()
	logf, err := os.CreateTemp(dir, LogFilePattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logf.Close() })
	prev := logFileSink.Swap(logf)
	t.Cleanup(func() { logFileSink.Store(prev) })

	parked, ready := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(parked) })
	go parkForStacks(ready, parked)
	<-ready

	name, err := WriteStacks()
	if err != nil {
		t.Fatalf("WriteStacks: %v", err)
	}
	if got := filepath.Dir(name); got != dir {
		t.Errorf("the stacks are in %s; want the log's folder, %s", got, dir)
	}
	if ok, _ := filepath.Match(StacksFilePattern, filepath.Base(name)); !ok {
		t.Errorf("the stacks file is %s; want one named %s", filepath.Base(name), StacksFilePattern)
	}
	fi, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("the stacks file's mode is %v; want 0600, its owner's alone", fi.Mode().Perm())
	}
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	// Each stack under its goroutine's own header, with its state, as a panic
	// prints them: a stall is read from what each goroutine waits on. The
	// goroutine that wrote them is running.
	header := regexp.MustCompile(`^goroutine \d+ \[[^\]]+\]:\n`)
	for _, want := range []struct{ frame, state string }{
		{"TestWriteStacksWritesEveryGoroutinesStackBesideTheLog", "[running]"},
		{"parkForStacks", ""},
	} {
		var found bool
		for _, g := range strings.Split(string(b), "\n\n") {
			if !strings.Contains(g, want.frame+"(") { // a call, not a "created by" line
				continue
			}
			found = true
			if !header.MatchString(g) || !strings.Contains(strings.SplitN(g, "\n", 2)[0], want.state) {
				t.Errorf("the stack of %s has no goroutine header with its state %q:\n%s", want.frame, want.state, g)
			}
		}
		if !found {
			t.Errorf("the stacks file has no frame of %s:\n%s", want.frame, b)
		}
	}

	// A second call writes a second file, never over the first.
	again, err := WriteStacks()
	if err != nil {
		t.Fatalf("WriteStacks: %v", err)
	}
	if again == name {
		t.Errorf("a second WriteStacks wrote %s again", name)
	}
}

// With no log, the stacks go to the OS temp directory.
func TestWriteStacksWithNoLogWritesToTheTempDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	prev := logFileSink.Swap(nil)
	t.Cleanup(func() { logFileSink.Store(prev) })
	name, err := WriteStacks()
	if err != nil {
		t.Fatalf("WriteStacks: %v", err)
	}
	if runtime.GOOS != "windows" && filepath.Dir(name) != dir {
		t.Errorf("the stacks are in %s; want the temp directory, %s", filepath.Dir(name), dir)
	}
}

// parkForStacks says it has begun (ready), then parks until c closes. Its
// caller waits for ready before it looks: a goroutine that has not begun yet
// shows in the stacks as its go statement's wrapper, not as this function (1
// look in about 50 found it so).
//
//go:noinline
func parkForStacks(ready, c chan struct{}) {
	close(ready)
	<-c
}

// A log folder WriteStacks cannot create its file in is an error, naming no
// file; a file it could not write all of is named, with why.
func TestWriteStacksSaysWhatItCouldNotDo(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WriteStacks panicked: %v", r)
		}
	}()
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gone, err := os.CreateTemp(dir, LogFilePattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gone.Close() })
	// The log's folder is removed under the run.
	if err := os.Remove(gone.Name()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	prev := logFileSink.Swap(gone)
	t.Cleanup(func() { logFileSink.Store(prev) })
	if name, err := WriteStacks(); err == nil || name != "" {
		t.Errorf("WriteStacks in a folder that is gone wrote %q, err %v; want no file and an error", name, err)
	}

	logFileSink.Store(nil)
	t.Setenv("TMPDIR", t.TempDir())
	prevWrite := writeGoroutines
	writeGoroutines = func(io.Writer) error { return errors.New("short write") }
	t.Cleanup(func() { writeGoroutines = prevWrite })
	name, err := WriteStacks()
	if err == nil || !strings.Contains(err.Error(), "short write") || name == "" {
		t.Errorf("a stacks file that could not be written all: %q, err %v; want it named, with the write's error", name, err)
	}
}
