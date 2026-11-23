package log

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
)

// StacksFilePattern names the file WriteStacks writes: a fresh file beside
// this run's log (LogFilePattern).
const StacksFilePattern = "keploy-stacks-*.txt"

// WriteStacks writes the stack of every goroutine of this process, as a panic
// prints them, to a fresh file (StacksFilePattern) in the folder of this run's
// log, or in the OS temp directory when the run keeps none, and returns its
// name. Stacks only: each goroutine's frames and its arguments' words, never
// the bytes a connection carried. It is created as the log is, 0600 and with
// O_EXCL (os.CreateTemp), so nothing already at the name is written through,
// and handed, under sudo, to the user sudo ran keploy for (giveLogToSudoUser),
// who reads the log. Unlike the log, nothing removes it at the run's end: it is
// written for something the run may not get past, to be read after.
func WriteStacks() (string, error) {
	dir := os.TempDir()
	if f := logFileSink.Load(); f != nil {
		dir = filepath.Dir(f.Name())
	}
	f, err := os.CreateTemp(dir, StacksFilePattern)
	if err != nil {
		return "", err
	}
	// The file is written either way: under sudo, root reads it if its user
	// cannot.
	_ = giveLogToSudoUser(f)
	werr := writeGoroutines(f)
	return f.Name(), errors.Join(werr, f.Close())
}

// writeGoroutines writes every goroutine's stack to w, as a panic prints
// them. A test swaps in one that fails.
var writeGoroutines = func(w io.Writer) error { return pprof.Lookup("goroutine").WriteTo(w, 2) }
