// Package safeyaml reads a file keploy takes from the repository it runs in
// without letting the file block keploy or run it out of memory.
//
// keploy reads several files straight out of the repository it runs in:
// keploy.yml (every command, in PreProcessFlags), a set's last-replay.yaml
// (ReadReceipt) and a run's report (reportdb.GetReport). git commits symlinks,
// so any of those can be a link to anything on the machine, and a cloned repo
// is untrusted input. Read to EOF with the standard library, a link to
// /dev/zero runs the command out of memory, a FIFO blocks it for good -- the
// open as well as the read -- and /proc/self/pagemap, which stats as a regular
// file of size zero, does both slowly.
//
// So keploy reads such a file only through ReadFile, which opens the path
// without blocking, asks the OPEN file what it is -- a stat of the path
// answers for whatever the path named a moment earlier -- refuses anything but
// a regular file, refuses one larger than the caller's limit, and reads
// exactly the size the open file reported. A file that holds more or less than
// that size is refused, never read to its end or cut short: a truncated YAML
// document can decode into something the file does not say.
package safeyaml

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// ErrNotRegular is returned when a path keploy meant to read as a file is a
// FIFO, a device, a socket or a directory. ErrNotDir is its counterpart for a
// path keploy meant to list. Callers may test for them with errors.Is.
var (
	ErrNotRegular = errors.New("not a regular file")
	ErrNotDir     = errors.New("not a directory")
	// ErrSizeChanged is a file that held more, or less, than the size it had
	// when it was opened: a procfs file, which reports size zero whatever it
	// holds, or one being written as keploy reads it.
	ErrSizeChanged = errors.New("it holds more or less than its size says")
)

// RefusedError is a file keploy did not read because it is past one of
// keploy's limits. It is not a fault in the file -- another tool may read it
// -- so it is its own type, distinct from a malformed or missing file.
type RefusedError struct{ msg string }

func (e RefusedError) Error() string { return e.msg }

// IsRefused reports whether err is (or wraps) a RefusedError.
func IsRefused(err error) bool {
	var r RefusedError
	return errors.As(err, &r)
}

// ReadFile reads the whole of the regular file p, when it holds at most limit
// bytes. A path that is not a regular file, or a file larger than limit, is an
// error, never a partial read.
func ReadFile(p string, limit int64) ([]byte, error) {
	f, info, err := open(p, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info.Size() > limit {
		return nil, RefusedError{fmt.Sprintf("larger than the %s keploy reads", byteSize(limit))}
	}
	return readAll(f, info.Size())
}

// readAll reads f, which reported size bytes when it was opened, requiring it
// to hold exactly that: no more (a growing or procfs file) and no less (a
// truncated one).
func readAll(f *os.File, size int64) ([]byte, error) {
	buf := make([]byte, size)
	if _, err := io.ReadFull(f, buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, ErrSizeChanged
		}
		return nil, err
	}
	var one [1]byte
	n, err := f.Read(one[:])
	switch {
	case n > 0:
		return nil, ErrSizeChanged
	case err != nil && !errors.Is(err, io.EOF):
		return nil, err
	}
	return buf, nil
}

// OpenDir opens the directory p for listing, without blocking, and refuses
// anything that is not a directory (ErrNotDir): os.Open of a FIFO in a
// directory's place blocks for good.
func OpenDir(p string) (*os.File, error) {
	f, _, err := open(p, true)
	return f, err
}

// open opens p when it is a regular file (or, with dir, a directory), checking
// both before and after the open so that keploy never opens anything else even
// for an instant.
func open(p string, dir bool) (*os.File, fs.FileInfo, error) {
	// Checked before the open too, so that keploy never opens anything else:
	// opening a device can act -- a serial port resets the board behind it --
	// whatever is read afterwards. The check that decides is openChecked's,
	// on the open descriptor, because the path can change under us.
	info, err := os.Stat(p)
	if err != nil {
		return nil, nil, err
	}
	if err := checkKind(info.Mode(), dir); err != nil {
		return nil, nil, err
	}
	return openChecked(p, dir)
}

// openChecked opens p without blocking, and checks what the OPEN file is.
func openChecked(p string, dir bool) (*os.File, fs.FileInfo, error) {
	// O_NONBLOCK: open blocks on a FIFO until something writes to it, and the
	// path can become one after any check of it. It changes nothing for a
	// regular file or a directory. O_NOCTTY: a terminal never becomes keploy's
	// controlling terminal. Windows ignores both.
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = checkKind(info.Mode(), dir)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// checkKind refuses anything but a regular file, or, with dir, a directory.
func checkKind(m fs.FileMode, dir bool) error {
	switch {
	case dir && !m.IsDir():
		return fmt.Errorf("%w: it is %s", ErrNotDir, kindOf(m))
	case !dir && !m.IsRegular():
		return fmt.Errorf("%w: it is %s", ErrNotRegular, kindOf(m))
	}
	return nil
}

func kindOf(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "a regular file"
	case m.IsDir():
		return "a directory"
	case m&fs.ModeNamedPipe != 0:
		return "a FIFO"
	case m&fs.ModeSocket != 0:
		return "a socket"
	case m&fs.ModeDevice != 0:
		return "a device"
	}
	return "an irregular file"
}

// StripPath returns err without the path an *fs.PathError carries, for a
// message that names the file itself: a path in a temporary checkout says
// nothing, and repeating the whole of it once per file is noise.
func StripPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// byteSize names a limit the way a person reads it: 4 MiB, 512 KiB.
func byteSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
