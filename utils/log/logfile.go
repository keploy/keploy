package log

import (
	"errors"
	"io"
	"os"
)

// LogFileName is the log every keploy run keeps beside the console, in the
// directory it runs in.
//
// A run writes it only when it is safely this run's: opened without following
// a symlink at that name, a regular file with no other name (a hard link would
// make it some other file), and owned by the user running keploy, by the user
// who ran sudo, or by whoever owns the directory. The directory is often a
// repository someone else wrote -- git keeps symlinks -- and the agent runs as
// root: a keploy-logs.txt linked to /etc/passwd used to be opened, appended to
// and chmod-ed 0777 by path, which left the link's target world-writable.
// Anything else at that name is left alone, and the run logs to the console
// only, saying why.
const LogFileName = "keploy-logs.txt"

// logFileMode is what a new log file is created with: its owner writes it,
// others may read it. It is never set by path, and an existing log writable by
// others (as every one this used to create was: 0777) is narrowed through the
// open file.
const logFileMode = 0o644

// IsLogFile reports whether LogFileName in the working directory is still own,
// the log this run opened (its os.FileInfo, taken while it was open), so a
// run removes only its own log, never a file another put at that name.
func IsLogFile(own os.FileInfo) bool {
	if own == nil {
		return false
	}
	cur, err := os.Lstat(LogFileName)
	return err == nil && os.SameFile(own, cur)
}

// ReadLogFile returns what f, the log this run opened, holds. It reads it
// through LogFileName, without following a symlink there, and only while that
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
	r, err := os.OpenFile(LogFileName, os.O_RDONLY|oNoFollow|oNonBlock, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	cur, err := r.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(own, cur) {
		return nil, errors.New(LogFileName + " is no longer the log this run opened")
	}
	return io.ReadAll(r)
}
