package log

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LogFilePattern names the log every keploy run keeps beside the console: a
// file the run creates fresh in the OS temp directory (os.TempDir: TMPDIR on
// Unix, the per-user temp directory on macOS and Windows), at a name
// os.CreateTemp picks, and removes at a normal exit. A run that failed keeps
// it there instead (KeepLogFile), for the bug report it is wanted for, and
// says where; the next run's start prunes the kept ones (pruneLogFiles).
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
// removes or keeps it: the CLI ends it, at its own end, by its exit code
// (utils.EndRunLog). Otherwise the agent keeps a log of its own
// (LogFilePattern) and says where, once.
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

// ownLogs holds each log this process created (createLogFile) and has not
// closed, with what it was when created: CloseLogFile closes and removes only
// a log it holds, and takes it out first, so a second close of the same log
// is a no-op on every platform. Nothing is asked of a closed file: on
// Windows, a closed file's Stat is an error of its own, not os.ErrClosed.
var ownLogs sync.Map // *os.File -> os.FileInfo

// markedLogs holds each log of ownLogs this process marked live
// (markLiveLog): a run whose own log could not be marked prunes nothing.
var markedLogs sync.Map // *os.File -> true

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
			// The agent marks the CLI's log live too: an agent that outlives
			// a CLI killed outright still writes it, and no run prunes it
			// while the agent has it open.
			markLive(f)
			borrowedLog.Store(f)
			return f, nil, nil, nil
		}
		refused = err
	}
	f, notHanded, err = createLogFile()
	if _, marked := markedLogs.Load(f); err == nil && marked {
		// A run's start prunes the logs failed runs kept beside its own
		// (pruneLogFiles), which it has marked live.
		pruneLogFiles(filepath.Dir(f.Name()), time.Now())
	}
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
	// Absolute, so the name the run says (and a failed run keeps) is one a
	// relative TMPDIR does not make relative to wherever it is read.
	dir := os.TempDir()
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
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
		own, err := f.Stat()
		if err != nil {
			_ = f.Close()
			_ = os.Remove(name)
			return nil, nil, err
		}
		// Marked as a live run's for as long as f is open, so that no other
		// run's start prunes it (pruneLogFiles). On a file system that keeps
		// no such marks this run prunes nothing (openLogFile): the others'
		// logs there are unmarked too.
		if markLive(f) {
			markedLogs.Store(f, true)
		}
		ownLogs.Store(f, own)
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
// f (a run that keeps no log), an f CloseLogFile already closed, and a file
// this process did not open as its log are no-ops: it tracks which logs it
// opened (ownLogs, borrowedLog) rather than ask f. The CLI's log an agent
// appends to (ClientLogFileFlag) is only closed: it is the CLI's to remove.
func CloseLogFile(f *os.File) error {
	if f == nil {
		return nil
	}
	logFileSink.CompareAndSwap(f, nil)
	if borrowedLog.CompareAndSwap(f, nil) {
		return f.Close()
	}
	own, ok := ownLogs.LoadAndDelete(f)
	if !ok {
		return nil
	}
	markedLogs.Delete(f)
	return errors.Join(f.Close(), removeIfStill(f.Name(), own.(os.FileInfo)))
}

// KeepLogFile ends f, this run's log, for a run whose log is wanted after it
// (a run that failed, for the bug report): it closes f and leaves it at its
// name, which it returns, made absolute, for the run to say. Only a log this
// process created is kept, and only while its name is still that file: ""
// is returned for nil, a log already ended, a file this process did not
// create as its log, and a log whose name was removed or replaced since
// (RemoveLogFile, before a re-exec). The CLI's log an agent appends to
// (ClientLogFileFlag) is only closed, as CloseLogFile closes it: it is the
// CLI's to keep or remove. Once closed, the log is no live run's, and a later
// run's start may prune it (pruneLogFiles).
func KeepLogFile(f *os.File) (string, error) {
	if f == nil {
		return "", nil
	}
	logFileSink.CompareAndSwap(f, nil)
	if borrowedLog.CompareAndSwap(f, nil) {
		return "", f.Close()
	}
	own, ok := ownLogs.LoadAndDelete(f)
	if !ok {
		return "", nil
	}
	markedLogs.Delete(f)
	// Asked while f is still open, so the file it names cannot be another
	// that took a freed inode.
	name := f.Name()
	cur, lerr := os.Lstat(name)
	still := lerr == nil && os.SameFile(own.(os.FileInfo), cur)
	if still {
		// Kept logs are pruned newest last by their time: a log kept now is
		// the newest, however long ago its last line was written. Stamped
		// through f, while it is still open and so still marked live: no
		// other run's start can prune it between its close and its stamp,
		// and nothing is looked up by its name.
		now := time.Now()
		_ = stampLog(f, now)
	}
	err := f.Close()
	if !still {
		return "", err
	}
	if abs, aerr := filepath.Abs(name); aerr == nil {
		name = abs
	}
	return name, err
}

// The bounds on the logs failed runs keep (KeepLogFile), which each run's
// start prunes (pruneLogFiles): a kept log goes once it is keptLogsFor old,
// or once keptLogsMax newer ones are kept beside it, and one start tries at
// most pruneMax of them, so a temp directory full of them, removable or not,
// costs one start no more. A log written to in the last pruneQuiet is never
// pruned, whatever its mark: a run marks its log just after creating it.
const (
	keptLogsFor = 7 * 24 * time.Hour
	keptLogsMax = 10
	pruneMax    = 100
	pruneQuiet  = 10 * time.Minute
)

// pruneLogFiles removes, from dir, the logs past keptLogsFor or keptLogsMax
// (above) that are no live run's, as of now, trying at most pruneMax. It only
// considers regular files (not links) whose name matches LogFilePattern and
// that belong to the user keploy runs for (logOwnedByRunUser). A log is a
// live run's while some process has it marked (markLiveLog): a live run's
// log, and its agent's, are never touched, nor another user's file. A killed
// run's mark went with it, so its log is pruned like any kept one. It returns
// how many it removed; what it cannot read or remove it leaves.
func pruneLogFiles(dir string, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	type kept struct {
		name string
		fi   os.FileInfo
	}
	var logs []kept
	for _, e := range entries {
		if ok, _ := filepath.Match(LogFilePattern, e.Name()); !ok {
			continue
		}
		name := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(name)
		if err != nil || !logOwnedByRunUser(fi) || now.Sub(fi.ModTime()) < pruneQuiet {
			continue
		}
		logs = append(logs, kept{name, fi})
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].fi.ModTime().After(logs[j].fi.ModTime()) })
	removed, tried := 0, 0
	for i, l := range logs {
		if tried == pruneMax {
			break
		}
		if i < keptLogsMax && now.Sub(l.fi.ModTime()) < keptLogsFor {
			continue
		}
		tried++
		if removeDead(l.name, l.fi) == nil {
			removed++
		}
	}
	return removed
}

// removeDead is removeDeadLog, and markLive markLiveLog; a test swaps in its
// own.
var (
	removeDead = removeDeadLog
	markLive   = markLiveLog
)

// RemoveLogFile removes f, this run's log, by its name, and only while that
// name is still the file this run created, so a run never removes a file
// another put there. f stays open: what is still written to it goes nowhere
// once f is closed, as a process that is about to exec into another does.
// The CLI's log an agent appends to is the CLI's to remove, never the agent's,
// and a file this process did not create as its log (ownLogs) is not its to
// remove either.
func RemoveLogFile(f *os.File) error {
	if f == nil || borrowedLog.Load() == f {
		return nil
	}
	own, ok := ownLogs.Load(f)
	if !ok {
		return nil
	}
	return removeIfStill(f.Name(), own.(os.FileInfo))
}

// ReviveLogFile gives this run's log a name again after an exec that failed.
// Before the exec, the log's name was removed (RemoveLogFile) for the process
// the run was to become; the exec failed, so the run goes on and ends in this
// process, and a failed run keeps its log (KeepLogFile). ReviveLogFile creates
// a fresh log (createLogFile), copies into it everything f holds, makes it the
// log the run writes, closes f, and returns the new log. It returns f
// unchanged when f still has its name or is not a log this run created, and
// f with an error when the copy cannot be made; the run then goes on writing
// to f, which has no name.
func ReviveLogFile(f *os.File) (*os.File, error) {
	if f == nil || borrowedLog.Load() == f {
		return f, nil
	}
	own, ok := ownLogs.Load(f)
	if !ok {
		return f, nil
	}
	if cur, err := os.Lstat(f.Name()); err == nil && os.SameFile(own.(os.FileInfo), cur) {
		return f, nil
	}
	held, err := f.Stat()
	if err != nil {
		return f, err
	}
	nf, _, err := createLogFile()
	if err != nil {
		return f, err
	}
	if _, err := io.Copy(nf, io.NewSectionReader(f, 0, held.Size())); err != nil {
		_ = CloseLogFile(nf)
		return f, err
	}
	logFileSink.CompareAndSwap(f, nf)
	ownLogs.Delete(f)
	markedLogs.Delete(f)
	_ = f.Close()
	return nf, nil
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
