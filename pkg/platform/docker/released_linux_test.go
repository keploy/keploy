//go:build linux

package docker

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// pod makes this process name a pod, whatever PID namespace the test runs
// in: its PID namespace reads as one of its own (a container's), not the
// host's, with PID 1 as this namespace has it.
func pod(t *testing.T) {
	t.Helper()
	orig, origIPC := pidNSInode, ipcNSInode
	pidNSInode = func() (uint64, error) { return 4026531999, nil }
	ipcNSInode = func() (uint64, error) { return 4026532001, nil }
	t.Cleanup(func() { pidNSInode, ipcNSInode = orig, origIPC })
	if _, err := podNote(); err != nil {
		t.Fatal(err)
	}
}

// A note an agent of this pod wrote is this pod's; none, another pod's, or
// what is not a note (a link, a damaged file) is not.
func TestAReleasedNoteIsReadOnlyAsThisPods(t *testing.T) {
	pod(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "keploy-app-released")
	if got, err := AppReleasedInThisPod(path); got || err != nil {
		t.Fatalf("no note: %v, %v; want false", got, err)
	}
	if err := MarkAppReleased(path); err != nil {
		t.Fatal(err)
	}
	if got, err := AppReleasedInThisPod(path); !got || err != nil {
		t.Fatalf("this pod's note: %v, %v; want true", got, err)
	}
	if err := os.WriteFile(path, []byte("keploy-app-released v1 pidns=1 init=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := AppReleasedInThisPod(path); got {
		t.Fatal("another pod's note was taken for this pod's")
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	note, _ := podNote()
	if err := os.WriteFile(target, []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if got, _ := AppReleasedInThisPod(path); got {
		t.Fatal("a note was read through a link")
	}
	if err := MarkAppReleased(path); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the note did not replace the link: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(target); string(b) != note {
		t.Fatalf("the write went through the link: %q", b)
	}
}

// A FIFO at the note's place, whose writer wrote this pod's note and left
// while another reader holds it open, is not read as a note: only a regular
// file is.
func TestAFIFOWhoseWriterLeftIsNotReadAsAReleasedNote(t *testing.T) {
	pod(t)
	path := filepath.Join(t.TempDir(), "keploy-app-released")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	note, _ := podNote()
	if _, err := w.WriteString(note); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if got, _ := AppReleasedInThisPod(path); got {
		t.Fatal("a FIFO holding this pod's note was read as the note")
	}
}

// The note is kept in the pod's /dev/shm, which every container of the pod
// shares and which outlives a restarted container, in the classic sidecar and
// the native one alike; not on the keploy-tls volume, which a native
// (low-latency) sidecar does not have, so a note there was lost with the
// container's own layer.
func TestTheReleasedNoteIsInThePodsSharedMemory(t *testing.T) {
	if AppReleasedFile != "/dev/shm/keploy-app-released" {
		t.Fatalf("AppReleasedFile = %s, want /dev/shm/keploy-app-released", AppReleasedFile)
	}
}

// A note written over a leftover temporary file of another's (a FIFO, a
// link, a file of another content) replaces it with one of its own, never
// writing through or into it, and leaves no temporary file behind.
func TestAReleasedNoteIsWrittenOverWhatALeftoverTemporaryFileIs(t *testing.T) {
	pod(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "keploy-app-released")
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, leave := range []func(string) error{
		func(p string) error { return os.Symlink(target, p) },
		func(p string) error { return syscall.Mkfifo(p, 0o600) },
		func(p string) error { return os.WriteFile(p, []byte("half a note"), 0o644) },
	} {
		if err := leave(path + ".tmp"); err != nil {
			t.Fatal(err)
		}
		if err := MarkAppReleased(path); err != nil {
			t.Fatal(err)
		}
		if got, err := AppReleasedInThisPod(path); !got || err != nil {
			t.Fatalf("this pod's note: %v, %v; want true", got, err)
		}
		if _, err := os.Lstat(path + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("a temporary file was left behind: %v", err)
		}
		if b, _ := os.ReadFile(target); string(b) != "keep" {
			t.Fatalf("the write went through the leftover link: %q", b)
		}
	}
}

// The host's IPC namespace names no pod: its /dev/shm outlives every pod, so
// no note is written or taken there.
func TestTheHostsIPCNamespaceWritesAndTakesNoNote(t *testing.T) {
	pod(t)
	path := filepath.Join(t.TempDir(), "keploy-app-released")
	note, _ := podNote()
	if err := os.WriteFile(path, []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	ipcNSInode = func() (uint64, error) { return procIPCInitIno, nil } // pod's Cleanup restores it
	if got, _ := AppReleasedInThisPod(path); got {
		t.Fatal("a note was taken in the host's IPC namespace")
	}
	_ = os.Remove(path)
	if err := MarkAppReleased(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a note was written in the host's IPC namespace: %v", err)
	}
}

// Without the directory nothing is written.
func TestNoVolumeNoReleasedNote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "keploy-app-released")
	if err := MarkAppReleased(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a note was written without the volume: %v", err)
	}
}

// The host's PID namespace names no pod: it outlives every run, so a note
// there would release the next run's application before its agent was set
// up. Nothing is written there, and nothing is taken.
func TestTheHostsPIDNamespaceWritesAndTakesNoNote(t *testing.T) {
	pod(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "keploy-app-released")
	note, _ := podNote()
	if err := os.WriteFile(path, []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	pidNSInode = func() (uint64, error) { return procPIDInitIno, nil } // pod's Cleanup restores it
	if got, _ := AppReleasedInThisPod(path); got {
		t.Fatal("a note was taken in the host's PID namespace")
	}
	_ = os.Remove(path)
	if err := MarkAppReleased(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a note was written in the host's PID namespace: %v", err)
	}
}
