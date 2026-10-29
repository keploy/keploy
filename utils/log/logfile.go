package log

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
// none of those: createLogFile opens its name with O_EXCL, as os.CreateTemp
// does, so a file, a symlink or a named pipe already there is refused and
// another name tried, and it creates the file 0600, its owner's alone. Under
// sudo, which drops TMPDIR, it is in /tmp, and its owner is the user sudo ran
// keploy for, to whom the run hands it by its descriptor at once
// (giveLogToSudoUser).
//
// The run says where it is beside its version line (the `logs:` line of
// cli/provider's PrintLogo), so a user who wants it can find it while the
// run lives.
//
// A native agent the CLI starts writes the CLI's log, not one of its own
// (ClientLogFileFlag): one run, one file.
const LogFilePattern = "keploy-logs-*.txt"

// ClientLogFileFlag is the `keploy agent` flag by which the CLI that starts a
// native agent names its own log, for the agent to append its lines to. The
// agent takes it only where openClientLogFile's checks pass, and never
// removes it: the CLI does, at its own end. Otherwise the agent keeps a log
// of its own (LogFilePattern) and says where, once.
const ClientLogFileFlag = "client-log-file"

// commandLine is os.Args, which New reads ClientLogFileFlag from: the log is
// opened before the command line is parsed. A test swaps in its own.
var commandLine = func() []string { return os.Args }

// openFile is os.OpenFile; a test swaps in one that changes the name first,
// as a name can change between openClientLogFile's checks.
var openFile = os.OpenFile

// borrowedLog is the CLI's log this agent appends to (ClientLogFileFlag),
// which CloseLogFile closes without removing it; nil when the run's log is its
// own.
var borrowedLog atomic.Pointer[os.File]

// openLogFile opens this run's log: the CLI's, for a native agent it names it
// to (ClientLogFileFlag) when the file passes openClientLogFile's checks, a
// fresh one (createLogFile) otherwise. refused is why a log the command line
// named was not taken, and notHanded why a log this run created under sudo was
// not handed to the user sudo ran keploy for (createLogFile). A temp
// directory the run cannot create its own log in is an error, which New turns
// into a console-only run: the run loses the file, not the command.
func openLogFile() (f *os.File, refused, notHanded, err error) {
	if path, ok := clientLogFileArg(commandLine()); ok {
		f, err := openClientLogFile(path)
		if err == nil {
			borrowedLog.Store(f)
			return f, nil, nil, nil
		}
		refused = err
	}
	f, notHanded, err = createLogFile()
	return f, refused, notHanded, err
}

// clientLogFileArg is the path args, a command line, gives ClientLogFileFlag:
// only on `keploy agent`, as --client-log-file=PATH or --client-log-file
// PATH, the last one before a "--".
func clientLogFileArg(args []string) (string, bool) {
	if len(args) < 2 || args[1] != "agent" {
		return "", false
	}
	flag := "--" + ClientLogFileFlag
	path := ""
	for i := 2; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			path = v
		} else if a == flag && i+1 < len(args) {
			path = args[i+1]
			i++
		}
	}
	return path, path != ""
}

// logFileInfix is the part of a log's name that LogFilePattern's "*" stands
// for, a new random one at each call, as os.CreateTemp picks; a test swaps in
// one that gives names it has put something at.
var logFileInfix = func() string { return strconv.FormatUint(uint64(rand.Uint32()), 10) }

// createLogFile creates a fresh log in the OS temp directory, as os.CreateTemp
// does (O_EXCL at a random name: a file, a link or a pipe already at a name is
// left alone and another name tried), but open for appending: the native
// agent appends to it too, and a write at this run's own offset would
// overwrite the agent's lines. Its mode is logFileMode whatever the umask, so
// that only a full log is ever sealed (logFileRoom). Under sudo it hands the
// file to the user sudo ran keploy for (giveLogToSudoUser), so that user can
// read the log the run names, and the agent, which takes only that user's
// file, appends to it. notHanded is why it could not, if it could not; the
// run keeps the file.
func createLogFile() (f *os.File, notHanded, err error) {
	prefix, suffix, _ := strings.Cut(LogFilePattern, "*")
	dir := os.TempDir()
	for try := 0; ; try++ {
		name := filepath.Join(dir, prefix+logFileInfix()+suffix)
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_APPEND, logFileMode)
		if errors.Is(err, os.ErrExist) && try < 10000 {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if err := f.Chmod(logFileMode); err != nil {
			_ = f.Close()
			_ = os.Remove(name)
			return nil, nil, err
		}
		return f, giveLogToSudoUser(f), nil
	}
}

// ReadLogFile returns what f, the log this run created, holds (openOwnLog).
func ReadLogFile(f *os.File) ([]byte, error) {
	r, err := openOwnLog(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

// ReadLogFileTail returns the last limit bytes f, the log this run created,
// holds (openOwnLog), from the start of a line where it cuts the log and a
// line starts within them: a crash
// report sends what the log ends with, which a log of up to FileCapBytes
// cannot all fit in, and it is read into memory no bigger than limit.
func ReadLogFileTail(f *os.File, limit int64) ([]byte, error) {
	r, err := openOwnLog(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	fi, err := r.Stat()
	if err != nil {
		return nil, err
	}
	from := max(fi.Size()-limit, 0)
	b := make([]byte, fi.Size()-from)
	n, err := r.ReadAt(b, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	b = b[:n]
	if from > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 && i < len(b)-1 {
			b = b[i+1:]
		}
	}
	return b, nil
}

// openOwnLog opens f, the log this run created, to read, through f's name,
// without following a symlink there, and only while that name is still f: a
// crash report must not read whatever the name points at now.
func openOwnLog(f *os.File) (*os.File, error) {
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
	cur, err := r.Stat()
	if err == nil && !os.SameFile(own, cur) {
		err = errors.New(f.Name() + " is no longer the log this run created")
	}
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

// CloseLogFile closes f, this run's log, and removes it (RemoveLogFile). A nil
// f, a run that keeps no log, and an f already closed are no-ops. The CLI's
// log an agent appends to (ClientLogFileFlag) is only closed: it is the CLI's
// to remove.
func CloseLogFile(f *os.File) error {
	if f == nil {
		return nil
	}
	logFileSink.CompareAndSwap(f, nil)
	if borrowedLog.CompareAndSwap(f, nil) {
		return f.Close()
	}
	own, statErr := f.Stat()
	if errors.Is(statErr, os.ErrClosed) {
		return nil
	}
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
// The CLI's log an agent appends to is the CLI's to remove, never the agent's.
func RemoveLogFile(f *os.File) error {
	if f == nil || borrowedLog.Load() == f {
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
