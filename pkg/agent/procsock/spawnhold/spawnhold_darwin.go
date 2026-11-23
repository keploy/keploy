//go:build darwin

// Package spawnhold holds a posix_spawn in the kernel for a test: the state
// in which macOS's kern.proc sysctls, ps(1)'s and the ones keploy read
// before, wait for that spawn to end. It is for tests only.
package spawnhold

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/procsock"
)

// holderC spawns /usr/bin/true with a file action that opens argv[1], a FIFO,
// for writing. XNU runs that open inside the spawn, so the spawn and its
// child are held until something opens the FIFO for reading. The holder
// does that itself argv[2] seconds after it starts, so the spawn it holds
// ends then whatever happens to the test that started it: a test killed
// before its cleanup ran left a spawn held this way for good, and every ps
// and every keploy on the machine waiting on it.
const holderC = `#include <fcntl.h>
#include <pthread.h>
#include <signal.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/wait.h>
#include <unistd.h>
extern char **environ;
static const char *fifo;
static void *release(void *secs) {
  sleep((unsigned)atoi((const char *)secs));
  open(fifo, O_RDONLY | O_NONBLOCK);
  return NULL;
}
int main(int argc, char **argv) {
  if (argc != 3 && argc != 4) return 2;
  if (argc == 4) signal(SIGINT, SIG_IGN);
  fifo = argv[1];
  pthread_t th;
  if (pthread_create(&th, NULL, release, argv[2]) != 0) return 2;
  posix_spawn_file_actions_t fa;
  posix_spawn_file_actions_init(&fa);
  posix_spawn_file_actions_addopen(&fa, 3, fifo, O_WRONLY, 0);
  char *child[] = {"/usr/bin/true", NULL};
  pid_t pid = 0;
  printf("spawning\n");
  fflush(stdout);
  int rc = posix_spawn(&pid, "/usr/bin/true", &fa, NULL, child, environ);
  if (rc == 0) waitpid(pid, NULL, 0);
  printf("spawned %d\n", rc);
  fflush(stdout);
  return rc == 0 ? 0 : 1;
}
`

// selfRelease is how long the holder holds its spawn at most.
const selfRelease = 60 * time.Second

// Held is a spawn held in the kernel.
type Held struct {
	// Holder is the process whose posix_spawn is held, and Child the
	// child that spawn is creating.
	Holder, Child int
	fifo          string
	out           *bufio.Reader
	cmd           *exec.Cmd
	// stopped: the test ends the holder itself (HoldInGroup), and Release
	// only makes sure nothing stays held or runs.
	stopped bool
}

// Hold starts a holder and returns once its spawn is in the kernel. The
// cleanup releases the spawn and waits for the holder. It skips the test
// where no C compiler is installed.
func Hold(t *testing.T) *Held {
	t.Helper()
	return hold(t, false, false)
}

// HoldInGroup is Hold with a holder that leads a process group of its own,
// as keploy starts an app, for a test that stops it: ignoreSIGINT has it
// ignore SIGINT. Its cleanup releases the spawn and kills the holder by its
// pid, whatever the test left.
func HoldInGroup(t *testing.T, ignoreSIGINT bool) *Held {
	t.Helper()
	return hold(t, true, ignoreSIGINT)
}

func hold(t *testing.T, group, ignoreSIGINT bool) *Held {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler (cc) to build the spawn holder")
	}
	dir := t.TempDir()
	src, bin, fifo := filepath.Join(dir, "holder.c"), filepath.Join(dir, "holder"), filepath.Join(dir, "fifo")
	if err := os.WriteFile(src, []byte(holderC), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(cc, "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build the spawn holder: %v: %s", err, out)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{fifo, strconv.Itoa(int(selfRelease / time.Second))}
	if ignoreSIGINT {
		args = append(args, "ignore-sigint")
	}
	cmd := exec.Command(bin, args...)
	if group {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &Held{Holder: cmd.Process.Pid, fifo: fifo, out: bufio.NewReader(pipe), cmd: cmd, stopped: group}
	// Registered after the holder starts, so it runs before t.TempDir's
	// removal: the FIFO is opened while it is still there.
	t.Cleanup(func() { h.Release(t) })
	if l, err := h.out.ReadString('\n'); err != nil || strings.TrimSpace(l) != "spawning" {
		t.Fatalf("the spawn holder said %q, %v; want spawning", l, err)
	}
	// The spawn is held once its child is in the process table and the
	// holder has not said it spawned.
	for deadline := time.Now().Add(10 * time.Second); h.Child == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the spawn holder's child never appeared")
		}
		pids, err := procsock.PIDs()
		if err != nil {
			t.Fatal(err)
		}
		for _, pid := range pids {
			if p, err := procsock.Info(pid); err == nil && p.PPID == h.Holder {
				h.Child = pid
			}
		}
	}
	if h.out.Buffered() > 0 {
		t.Fatal("the spawn holder said more before its spawn was released")
	}
	return h
}

// Release opens the FIFO for reading, which ends the held spawn, and waits
// for the holder to say it spawned and exit. A second call does nothing.
func (h *Held) Release(t *testing.T) {
	t.Helper()
	if h.cmd == nil {
		return
	}
	cmd := h.cmd
	h.cmd = nil
	r, err := os.OpenFile(h.fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Errorf("open the spawn holder's FIFO to release its spawn: %v", err)
	} else {
		defer func() { _ = r.Close() }()
	}
	if h.stopped {
		// The test has stopped it, or tried: it is gone, or ends now.
		_ = syscall.Kill(h.Holder, syscall.SIGKILL)
		_ = cmd.Wait()
		return
	}
	if l, err := h.out.ReadString('\n'); err != nil || strings.TrimSpace(l) != "spawned 0" {
		t.Errorf("the released spawn holder said %q, %v; want spawned 0", l, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("the spawn holder: %v", err)
	}
}
