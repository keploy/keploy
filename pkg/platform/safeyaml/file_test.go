package safeyaml

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// withDeadline runs f and fails if it does not return within d: a FIFO or a
// device that the guard did NOT refuse would block or run long, so every case
// that feeds one is wrapped in this. It is what proves the guard returns
// promptly rather than merely returns the right error.
func withDeadline(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not return within %s -- the guard blocked", d)
	}
}

func TestReadFileRegular(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.yaml")
	want := []byte("set: orders\nat: 2026-01-01T00:00:00Z\n")
	if err := os.WriteFile(p, want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(p, 1<<20)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("ReadFile = %q, want %q", got, want)
	}
}

func TestReadFileRefusesFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "keploy.yml")
	mkfifo(t, p)
	// A FIFO opened for reading with no writer blocks the open, and reads
	// forever after: the whole point is that this returns AT ONCE with an
	// error, so it is deadline-bounded.
	withDeadline(t, 5*time.Second, func() {
		_, err := ReadFile(p, 1<<20)
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("ReadFile(FIFO) = %v, want ErrNotRegular", err)
		}
	})
}

// ReadFile never opens a FIFO, even for an instant: opening one acts -- a
// writer waiting to open it is let through -- as opening a device can. The
// kind is checked before the open as well as on what was opened. Here a
// writer waits in its open of the FIFO; ReadFile must refuse the FIFO and
// leave the writer waiting, until the test opens it to read itself.
// Mutation: drop the check before the open.
func TestReadFileNeverOpensAFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on windows")
	}
	p := filepath.Join(t.TempDir(), "keploy.yml")
	mkfifo(t, p)
	writer := make(chan *os.File, 1)
	go func() {
		// Blocks until something opens the FIFO to read.
		if f, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
			writer <- f
		}
	}()
	// Time for the writer to reach its open; were it late, a ReadFile that
	// opened the FIFO would go unseen, but the test could not fail wrongly.
	time.Sleep(200 * time.Millisecond)
	withDeadline(t, 5*time.Second, func() {
		if _, err := ReadFile(p, 1<<20); !errors.Is(err, ErrNotRegular) {
			t.Errorf("ReadFile(FIFO) = %v, want ErrNotRegular", err)
		}
	})
	select {
	case f := <-writer:
		_ = f.Close()
		t.Fatal("ReadFile opened the FIFO: the writer waiting on it got through")
	case <-time.After(500 * time.Millisecond):
	}
	r, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	select {
	case f := <-writer:
		_ = f.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never got through, even with the test reading")
	}
}

func TestReadFileRefusesDeviceSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/zero on windows")
	}
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("no /dev/zero")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "keploy.yml")
	if err := os.Symlink("/dev/zero", p); err != nil {
		t.Fatal(err)
	}
	// Reading /dev/zero to EOF never ends and exhausts memory; the guard must
	// refuse it without reading a byte, promptly.
	withDeadline(t, 5*time.Second, func() {
		_, err := ReadFile(p, 1<<20)
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("ReadFile(/dev/zero) = %v, want ErrNotRegular", err)
		}
	})
}

func TestReadFileRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "keploy.yaml")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(sub, 1<<20); !errors.Is(err, ErrNotRegular) {
		t.Errorf("ReadFile(dir) = %v, want ErrNotRegular", err)
	}
}

func TestReadFileTooLarge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(p, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadFile(p, 1024)
	if !IsRefused(err) {
		t.Fatalf("ReadFile over the limit = %v, want RefusedError", err)
	}
	// And it names the limit a person can read.
	if !strings.Contains(err.Error(), "1 KiB") {
		t.Errorf("message %q does not name the limit", err)
	}
	// A file exactly at the limit is fine.
	if _, err := ReadFile(p, 2048); err != nil {
		t.Errorf("ReadFile at the limit: %v", err)
	}
}

// TestReadFileGrowsPastSize covers a file (a procfs file, or one being written
// as keploy reads it) that holds more than the size it reported when opened:
// keploy refuses it rather than decoding a prefix.
func TestReadFileGrowsPastSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "grow.yaml")
	if err := os.WriteFile(p, []byte("set: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, info, err := open(p, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	// Append after the size was taken at the open.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("extra: bytes\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := readAll(r, info.Size()); !errors.Is(err, ErrSizeChanged) {
		t.Fatalf("readAll of a grown file = %v, want ErrSizeChanged", err)
	}
}

// TestReadFileShrinksBelowSize covers a file that holds less than the size it
// reported when opened: keploy refuses it rather than decoding a document the
// file no longer contains.
func TestReadFileShrinksBelowSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shrink.yaml")
	if err := os.WriteFile(p, []byte("set: orders and more bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, info, err := open(p, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	// Truncate after the size was taken at the open.
	if err := os.Truncate(p, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(r, info.Size()); !errors.Is(err, ErrSizeChanged) {
		t.Fatalf("readAll of a shrunk file = %v, want ErrSizeChanged", err)
	}
}

// TestReadFileProcfs is the concrete case that named this guard: a file that
// stats as a regular file of size zero but yields bytes when read.
func TestReadFileProcfs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is linux-only")
	}
	const procfs = "/proc/self/maps"
	info, err := os.Stat(procfs)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Skipf("%s is not a size-zero regular file here", procfs)
	}
	withDeadline(t, 5*time.Second, func() {
		if _, err := ReadFile(procfs, 1<<20); !errors.Is(err, ErrSizeChanged) {
			t.Errorf("ReadFile(%s) = %v, want ErrSizeChanged", procfs, err)
		}
	})
}

func TestOpenDir(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenDir(dir)
	if err != nil {
		t.Fatalf("OpenDir on a directory: %v", err)
	}
	_ = f.Close()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDir(p); !errors.Is(err, ErrNotDir) {
		t.Errorf("OpenDir on a file = %v, want ErrNotDir", err)
	}
}

// TestOpenCheckedDecidesOnTheOpenFile: the check that decides is the one on
// the OPEN descriptor, since the path can change between the stat before the
// open and the open itself. Called here without that earlier stat.
func TestOpenCheckedDecidesOnTheOpenFile(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := openChecked(dir, false); !errors.Is(err, ErrNotRegular) {
		t.Errorf("openChecked(directory, file) = %v, want ErrNotRegular", err)
	}
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openChecked(p, true); !errors.Is(err, ErrNotDir) {
		t.Errorf("openChecked(file, directory) = %v, want ErrNotDir", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	fifo := filepath.Join(dir, "fifo")
	mkfifo(t, fifo)
	withDeadline(t, 5*time.Second, func() {
		if _, _, err := openChecked(fifo, false); !errors.Is(err, ErrNotRegular) {
			t.Errorf("openChecked(FIFO) = %v, want ErrNotRegular", err)
		}
	})
}
