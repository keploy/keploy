//go:build linux

package linux

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// When a docker-mode agent's recording started.
//
// The sys_enter_socket tracepoint compares the start time of each process in
// the agent's PID namespace with agent_info.recording_start_time. A process
// that started before it was there before keploy and is excluded: its calls
// are never redirected. Any other process is the application. That is only
// right while recording_start_time is when keploy started recording in this
// PID namespace, and an agent process is not the recording. Kubernetes
// restarts a sidecar container in place (a crash, an OOM kill, a failed
// startup probe) while the pod's other containers, the application among
// them, go on running in the pod's PID namespace. An agent that stamped its
// own load time made the still-running application a process that was there
// before keploy, and from then on every call it made went to the real
// dependency: not recorded, not mocked, and nothing said so.
//
// The rule: recording_start_time is the CLOCK_BOOTTIME at which the first
// agent in this PID namespace loaded its hooks, and every later agent in the
// same PID namespace uses that same value.
//
// Why not the application's own start, as enterprise's proxyless filter
// anchors on its target container: this agent does not know which processes
// are the application. After a restart, the application and a sidecar that
// started before keploy did (an Istio proxy, what the filter exists to leave
// alone) are both older than the new agent. Only the side of the first
// agent's load each one started on tells them apart, and only the first agent
// saw that. So the first agent writes its load time down where every later
// agent of the same pod finds it: a file in /dev/shm, the IPC namespace's
// shared memory. Kubernetes gives every container of a pod the pod's IPC
// namespace, so the file outlives a restarted container and goes with the
// pod. Docker gives each container its own, which goes with the container.
//
// The file names the PID namespace and that namespace's init, and is used
// only by an agent whose PID namespace has the same inode and the same init.
// An init (PID 1) lives exactly as long as its namespace, so a file left by a
// namespace that is gone, whose inode the kernel has since given to a new
// one, is never taken for the new one's. So the record lasts as long as the
// PID namespace, not as long as one agent's session: in a long-lived
// container where `keploy agent --is-docker` is run again and again, a
// process started between two sessions is not excluded by the later one.
//
// Every container of the pod can write to /dev/shm. The file is read only if
// it is a regular file of this agent's user, of record size, and not through
// a link, and is used only if its start is no later than the moment this
// agent reads it, while it loads; anything else is replaced with this agent's
// load time. A value planted there can move the start earlier, never past
// this agent's load: it can make keploy capture more, never exclude a process
// that started after the agent loaded, as the application does.
//
// Nothing is written down, and the load time is used as before, where the
// namespace outlives any one recording: the host's PID namespace (an agent
// started with --is-docker outside a container, or a DaemonSet agent with
// hostPID, which does not attach the tracepoint anyway), and the host's IPC
// namespace, whose /dev/shm would keep a file per pod until the next reboot.

const (
	// procPIDInitInode and procIPCInitInode are PROC_PID_INIT_INO and
	// PROC_IPC_INIT_INO: the fixed inodes of the host's PID and IPC
	// namespaces.
	procPIDInitInode = 0xEFFFFFFC
	procIPCInitInode = 0xEFFFFFFF

	// recordingStartFile is the name of the file, in shmDir, followed by the
	// PID namespace's inode.
	recordingStartFile = "keploy-recording-start-"
)

// recordingStartSource is what recordingStart reads.
type recordingStartSource struct {
	// now is CLOCK_BOOTTIME in nanoseconds, the clock of the kernel's
	// task start_boottime.
	now func() (uint64, error)
	// pidNS and ipcNS are the inodes of the agent's PID and IPC namespaces.
	pidNS func() (uint64, error)
	ipcNS func() (uint64, error)
	// initStart is when PID 1 of the agent's PID namespace started, in
	// clock ticks since boot: field 22 of /proc/1/stat.
	initStart func() (uint64, error)
	// shmDir is where the first agent's load time is kept.
	shmDir string
}

// hostRecordingStart is recordingStartSource read from this process's kernel.
func hostRecordingStart() recordingStartSource {
	return recordingStartSource{
		now: func() (uint64, error) {
			var ts unix.Timespec
			if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
				return 0, err
			}
			return uint64(ts.Sec)*1e9 + uint64(ts.Nsec), nil
		},
		pidNS:     func() (uint64, error) { return nsInode("pid") },
		ipcNS:     func() (uint64, error) { return nsInode("ipc") },
		initStart: func() (uint64, error) { return procStartTicks("/proc/1/stat") },
		shmDir:    "/dev/shm",
	}
}

// recordingStart is recording_start_time for this load of the hooks, and 0,
// which turns the exclusion off, when the clock cannot be read.
func recordingStart(logger *zap.Logger, src recordingStartSource) uint64 {
	now, err := src.now()
	if err != nil {
		logger.Debug("failed to read CLOCK_BOOTTIME; pre-existing PID exclusion disabled", zap.Error(err))
		return 0
	}
	// fresh is the load time, used when the first agent's cannot be kept.
	fresh := func(why string, fields ...zap.Field) uint64 {
		logger.Info("recording start boottime set; an agent restarted while the application runs on will exclude the application: "+why,
			append(fields, zap.Uint64("ns", now))...)
		return now
	}

	pidNS, err := src.pidNS()
	if err != nil {
		return fresh("its PID namespace could not be read", zap.Error(err))
	}
	if pidNS == procPIDInitInode {
		logger.Info("recording start boottime set (host PID namespace)", zap.Uint64("ns", now))
		return now
	}
	ipcNS, err := src.ipcNS()
	if err != nil {
		return fresh("its IPC namespace could not be read", zap.Error(err))
	}
	if ipcNS == procIPCInitInode {
		return fresh("it shares the host's IPC namespace, whose /dev/shm outlives the pod")
	}
	initStart, err := src.initStart()
	if err != nil {
		return fresh("the start of PID 1 could not be read", zap.Error(err))
	}

	mine := recordingStartRecord{pidNS: pidNS, init: initStart, start: now}
	path := filepath.Join(src.shmDir, recordingStartFile+strconv.FormatUint(pidNS, 10))
	first, err := src.keep(path, mine)
	if err != nil {
		return fresh("its load time could not be kept in "+path, zap.Error(err))
	}
	if first != now {
		logger.Info("recording start boottime set to the first agent's in this PID namespace (this agent was restarted, or another one loaded first)",
			zap.Uint64("ns", first), zap.Uint64("loadedAt", now), zap.String("record", path))
		return first
	}
	logger.Info("recording start boottime set", zap.Uint64("ns", now), zap.String("record", path))
	return now
}

// keep returns the recording start this agent uses: that of the agent of the
// same PID namespace that kept one at path first, or, where none has, its
// own, which it keeps there for the agents after it.
//
// The record is written to a temporary file and hard-linked into place, so a
// reader never sees half a record, and when agents load at once the first
// link sets the value and the others, finding it there, take it. What else may be at path (a namespace's that is gone, a
// damaged or foreign file, a symbolic link, a FIFO) is replaced by a rename,
// which replaces the directory entry and never writes through it. A
// filesystem without hard links gets the plain rename too.
func (src recordingStartSource) keep(path string, mine recordingStartRecord) (uint64, error) {
	tmp, err := os.CreateTemp(src.shmDir, recordingStartFile+"*.tmp")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(mine.String())
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return 0, werr
	}
	err = os.Link(tmp.Name(), path)
	if err == nil {
		return mine.start, nil
	}
	if errors.Is(err, fs.ErrExist) {
		if first, ok := src.kept(path, mine); ok {
			return first, nil
		}
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, err
	}
	return mine.start, nil
}

// kept is the load time a previous agent of mine's PID namespace kept at
// path, if one did: a record of the same namespace inode and the same init,
// with a start no later than now. The clock is read again here, not taken
// from the start of the load: a record another agent kept since this one read
// the clock is not in the future.
func (src recordingStartSource) kept(path string, mine recordingStartRecord) (uint64, bool) {
	r, err := readRecordingStartFile(path)
	if err != nil {
		return 0, false
	}
	now, err := src.now()
	if err != nil || r.pidNS != mine.pidNS || r.init != mine.init || r.start == 0 || r.start > now {
		return 0, false
	}
	return r.start, true
}

// recordingStartRecord is the content of a recording-start file.
type recordingStartRecord struct {
	pidNS, init, start uint64
}

func (r recordingStartRecord) String() string {
	return fmt.Sprintf("keploy-recording-start v1 pidns=%d init=%d start=%d\n", r.pidNS, r.init, r.start)
}

// maxRecordingStartSize bounds what is read of a record: the longest one
// String writes is under 100 bytes.
const maxRecordingStartSize = 128

// readRecordingStartFile reads the record at path. /dev/shm is shared with
// every container of the pod, so what is there is only read if it is a
// regular file this agent's user owns, never through a symbolic link, never
// by waiting on a FIFO's writer, and never beyond maxRecordingStartSize bytes.
// Anything else is an error, which makes it a damaged record.
func readRecordingStartFile(path string) (recordingStartRecord, error) {
	var r recordingStartRecord
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return r, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return r, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return r, fmt.Errorf("%s is not a regular file (mode %#o)", path, st.Mode)
	}
	if euid := os.Geteuid(); st.Uid != uint32(euid) {
		return r, fmt.Errorf("%s is owned by uid %d, not %d", path, st.Uid, euid)
	}
	buf := make([]byte, maxRecordingStartSize+1)
	n := 0
	for n < len(buf) {
		m, err := unix.Read(fd, buf[n:])
		if err != nil {
			return r, err
		}
		if m == 0 {
			break
		}
		n += m
	}
	if n > maxRecordingStartSize {
		return r, fmt.Errorf("%s is larger than a record", path)
	}
	b := string(buf[:n])
	if _, err := fmt.Sscanf(b, "keploy-recording-start v1 pidns=%d init=%d start=%d\n", &r.pidNS, &r.init, &r.start); err != nil {
		return r, err
	}
	if r.String() != b {
		return r, fmt.Errorf("%s is not a record: %q", path, b)
	}
	return r, nil
}

// nsInode is the inode of this process's namespace of the given kind.
func nsInode(kind string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join("/proc/self/ns", kind), &st); err != nil {
		return 0, err
	}
	return st.Ino, nil
}

// procStartTicks is field 22 of a /proc/<pid>/stat file: when the process
// started, in clock ticks since boot. The command name (field 2) is in
// parentheses and may itself hold spaces and parentheses, so the fields are
// counted from after the last ')'.
func procStartTicks(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("%s: no command name", path)
	}
	fields := strings.Fields(s[i+1:]) // fields[0] is field 3, the state
	if len(fields) < 20 {
		return 0, fmt.Errorf("%s: %d fields after the command name, want at least 20", path, len(fields))
	}
	return strconv.ParseUint(fields[19], 10, 64)
}
