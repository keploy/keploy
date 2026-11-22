//go:build linux

package docker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/utils"
)

// The note names the pod by its PID namespace's inode and the start of that
// namespace's init (PID 1, which lives as long as the namespace), as the
// recording start does (pkg/agent/hooks/linux recording_start.go): Kubernetes
// gives a recorded pod one PID namespace (shareProcessNamespace), whose init
// is the pod's sandbox, so every agent container of the pod names the same
// one, and no other pod does, even one given the inode of a namespace that is
// gone. A container of its own PID namespace (docker compose) names its own,
// so a note a run left on a volume that outlives it is never the next run's.
// The host's PID namespace names no pod: it outlives every run, so no note is
// written or taken there; nor in the host's IPC namespace, whose /dev/shm
// outlives every pod.

// procPIDInitIno and procIPCInitIno are PROC_PID_INIT_INO and
// PROC_IPC_INIT_INO, the host PID and IPC namespaces' inodes.
const (
	procPIDInitIno = 0xEFFFFFFC
	procIPCInitIno = 0xEFFFFFFF
)

// maxAppReleasedSize bounds what is read of a note: the one podNote writes is
// under 80 bytes.
const maxAppReleasedSize = 128

// pidNSInode and ipcNSInode are the inodes of this process's PID and IPC
// namespaces; variables so a test runs as in the host's or a pod's.
var (
	pidNSInode = func() (uint64, error) { return nsInode("pid") }
	ipcNSInode = func() (uint64, error) { return nsInode("ipc") }
)

func nsInode(kind string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/self/ns/"+kind, &st); err != nil {
		return 0, err
	}
	return st.Ino, nil
}

// podNote is the note of this process's pod, or an error where it names none.
func podNote() (string, error) {
	ino, err := pidNSInode()
	if err != nil {
		return "", err
	}
	if ino == procPIDInitIno {
		return "", errors.New("the host's PID namespace names no pod")
	}
	ipc, err := ipcNSInode()
	if err != nil {
		return "", err
	}
	if ipc == procIPCInitIno {
		return "", errors.New("the host's IPC namespace names no pod: its /dev/shm outlives every pod")
	}
	b, err := os.ReadFile("/proc/1/stat")
	if err != nil {
		return "", err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", errors.New("unexpected /proc/1/stat")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return "", errors.New("unexpected /proc/1/stat")
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return "", fmt.Errorf("the start of PID 1: %w", err)
	}
	return fmt.Sprintf("keploy-app-released v1 pidns=%d init=%d\n", ino, start), nil
}

// MarkAppReleased notes at path that this pod's application was released. It
// writes nothing where path's directory is not there or the process names no
// pod. The note is written to path's temporary file (path.tmp, a file of
// this process's own, made afresh: whatever another container left there is
// removed first) and renamed into place, which replaces whatever was at path
// (a link included) without writing through it. One temporary name, so an
// agent killed between the two leaves one file behind, which the next write
// takes over.
func MarkAppReleased(path string) error {
	dir := filepath.Dir(path)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil
	}
	note, err := podNote()
	if err != nil {
		return nil
	}
	return utils.WriteShmFile(path, []byte(note))
}

// AppReleasedInThisPod reports whether the note at path says this pod's
// application was released. The volume is shared with the application, so
// the note is read only if it is a regular file of this process's user,
// never through a link, never by waiting on a FIFO's writer, and never beyond
// a note's size; a note of another pod, or none, is false.
func AppReleasedInThisPod(path string) (bool, error) {
	want, err := podNote()
	if err != nil {
		return false, nil
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) {
		return false, nil
	}
	buf := make([]byte, maxAppReleasedSize+1)
	n, err := unix.Read(fd, buf)
	if err != nil {
		return false, err
	}
	return n <= maxAppReleasedSize && string(buf[:n]) == want, nil
}
