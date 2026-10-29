package log

import (
	"errors"
	"io"
	"os"
)

// LogFilePattern names the log every keploy run keeps beside the console: a
// file the run creates fresh in the OS temp directory (os.TempDir: TMPDIR on
// Unix, the per-user temp directory on macOS and Windows), at a name
// os.CreateTemp picks, and removes at a normal exit.
//
// It used to be keploy-logs.txt in the directory keploy ran in, usually the
// user's project. A run that was killed left it there, where git could commit
// it (root-owned under sudo) and keploy mock record's source fingerprint
// counted it as uncommitted code; and the name was one a repository could
// commit as a symlink for a root run to write through. A fresh temp file is
// none of those: os.CreateTemp opens its name with O_EXCL, so a file, a
// symlink or a named pipe already there is refused and another name tried,
// and it creates the file 0600, its owner's alone. Under sudo, which drops
// TMPDIR, it is in /tmp and belongs to the run, root.
//
// The run says where it is beside its version line (utils.LogFilePathLine),
// so a user who wants it can find it while the run lives.
const LogFilePattern = "keploy-logs-*.txt"

// openLogFile creates this run's log (LogFilePattern). A temp directory the
// run cannot create it in is an error, which New turns into a console-only
// run: the run loses the file, not the command.
func openLogFile() (*os.File, error) {
	return os.CreateTemp("", LogFilePattern)
}

// ReadLogFile returns what f, the log this run created, holds. It reads it
// through f's name, without following a symlink there, and only while that
// name is still f: a crash report must not read whatever the name points at
// now.
func ReadLogFile(f *os.File) ([]byte, error) {
	if f == nil {
		return nil, errors.New("this run keeps no log file")
	}
	own, err := f.Stat()
	if err != nil {
		return nil, err
	}
	r, err := os.OpenFile(f.Name(), os.O_RDONLY|oNoFollow|oNonBlock, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	cur, err := r.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(own, cur) {
		return nil, errors.New(f.Name() + " is no longer the log this run created")
	}
	return io.ReadAll(r)
}

// CloseLogFile closes f, this run's log, and removes it (RemoveLogFile). A nil
// f, a run that keeps no log, is a no-op.
func CloseLogFile(f *os.File) error {
	if f == nil {
		return nil
	}
	own, statErr := f.Stat()
	closeErr := f.Close()
	if statErr != nil {
		return errors.Join(closeErr, statErr)
	}
	return errors.Join(closeErr, removeIfStill(f.Name(), own))
}

// RemoveLogFile removes f, this run's log, by its name, and only while that
// name is still the file this run created, so a run never removes a file
// another put there. f stays open: what is still written to it goes nowhere
// once f is closed, as a process that is about to exec into another does.
func RemoveLogFile(f *os.File) error {
	if f == nil {
		return nil
	}
	own, err := f.Stat()
	if err != nil {
		return err
	}
	return removeIfStill(f.Name(), own)
}

// removeIfStill removes name while it is still own.
func removeIfStill(name string, own os.FileInfo) error {
	cur, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(own, cur) {
		return nil
	}
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
