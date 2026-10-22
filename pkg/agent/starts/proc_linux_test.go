//go:build linux

package starts

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// sysProc reads a process's parent and start from its /proc/<pid>/stat
// (utils.ReadProcStat): this process's, as the kernel reports them.
func TestSysProcReadsThisProcess(t *testing.T) {
	self := uint32(os.Getpid())
	if p, ok := (sysProc{}).Parent(self); !ok || p != uint32(os.Getppid()) {
		t.Fatalf("Parent(self) = %d, %v; want %d", p, ok, os.Getppid())
	}
	born, ok := (sysProc{}).Birth(self)
	if !ok {
		t.Fatal("Birth(self) could not be read")
	}
	// This test binary started a moment ago. Its start time is in clock
	// ticks, and boot() is the uptime's own reading, so allow a second each way.
	if age := time.Since(born); age < -time.Second || age > 5*time.Minute {
		t.Fatalf("Birth(self) = %s, %s ago; want this test binary's start", born, age)
	}
	if _, ok := (sysProc{}).Parent(1 << 30); ok {
		t.Fatal("Parent read a pid no process has")
	}
}

// fakeProcRoot is a /proc for sysProc to read: its own PID namespace entry
// (self/ns/pid) naming ns, and one process, pid, named comm, with argv.
func fakeProcRoot(t *testing.T, ns string, pid uint32, comm string, argv ...string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "self", "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ns, filepath.Join(root, "self", "ns", "pid")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, strconv.FormatUint(uint64(pid), 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// The kernel's own PID namespace entry, and another one's (a container's).
var (
	hostPIDNS    = fmt.Sprintf("pid:[%d]", uint32(utils.InitialPIDNSInode))
	privatePIDNS = "pid:[4026532718]"
)

// keploy reads a process by the kernel pid its proxy reports only where that
// pid names it in keploy's own /proc (the initial PID namespace). In a PID
// namespace of its own, where the same number is an unrelated process of the
// namespace's, nothing is read: no name, the reason and the kernel pid on the
// mock's origin and the call's source, the reason said once in the record log
// with the remedy, and the unrelated process's command line never logged.
func TestAKernelPidIsReadOnlyWhereItIsKeploysOwn(t *testing.T) {
	const kernelPid = 2912345
	t.Run("collides in a PID namespace of keploy's own", func(t *testing.T) {
		root := fakeProcRoot(t, privatePIDNS, kernelPid, "bash", "/bin/bash", "-c", "token=hunter2")
		r := New(sysProc{root: root}, 0)
		core, logs := observer.New(zapcore.DebugLevel)
		r.SetLogger(zap.New(core))
		r.NoteOrigin("7", kernelPid)
		m := &models.Mock{Name: "mock-6", ConnectionID: "7", Origin: &models.Origin{Clock: "proxy:run-1"}}
		r.StampOrigin(m)
		want := models.Origin{Clock: "proxy:run-1", Unnamed: models.UnnamedPIDNamespace, PID: kernelPid}
		if m.Origin == nil || *m.Origin != want {
			t.Fatalf("origin %+v; want %+v: no name, the reason and the kernel pid", m.Origin, want)
		}
		if src := r.SourceOf(kernelPid); src == nil || *src != (models.Origin{Unnamed: models.UnnamedPIDNamespace, PID: kernelPid}) {
			t.Fatalf("SourceOf = %+v; want no name, the reason and the kernel pid", src)
		}
		if comm, _, ok := r.ProcessOf(kernelPid); ok || comm != "" {
			t.Fatalf("ProcessOf = %q, %v; want nothing read", comm, ok)
		}
		if text := m.Origin.ProcessText(); strings.Contains(text, "bash") || !strings.Contains(text, "keploy runs in a PID namespace of its own") || !strings.Contains(text, "kernel pid 2912345") {
			t.Fatalf("the reason line names %q", text)
		}
		if n := logs.FilterMessage("a recorded call came from this process").Len(); n != 0 {
			t.Fatalf("an unrelated process's command line was logged %d times", n)
		}
		said := logs.FilterMessageSnippet("keploy runs in a PID namespace of its own").All()
		if len(said) != 1 || said[0].Level != zapcore.WarnLevel || said[0].ContextMap()["next_step"] == nil {
			t.Fatalf("the record log said the namespace %d times (%+v); want once, at warn, with a next step", len(said), said)
		}
		// The persisted origin is the clock alone.
		if p := m.Origin.Persisted(); p == nil || *p != (models.Origin{Clock: "proxy:run-1"}) {
			t.Fatalf("persisted origin %+v; want the clock alone", p)
		}
	})
	t.Run("keploy's own", func(t *testing.T) {
		root := fakeProcRoot(t, hostPIDNS, kernelPid, "sqs", "./sqs", "--queue", "orders")
		r := New(sysProc{root: root}, 0)
		r.NoteOrigin("7", kernelPid)
		m := &models.Mock{Name: "mock-6", ConnectionID: "7", Origin: &models.Origin{Clock: "proxy:run-1"}}
		r.StampOrigin(m)
		if m.Origin == nil || *m.Origin != (models.Origin{Clock: "proxy:run-1", Comm: "sqs", Exe: "./sqs"}) {
			t.Fatalf("origin %+v; want sqs, ./sqs", m.Origin)
		}
		if src := r.SourceOf(kernelPid); src == nil || *src != (models.Origin{Comm: "sqs", Exe: "./sqs"}) {
			t.Fatalf("SourceOf = %+v; want sqs, ./sqs", src)
		}
	})
	t.Run("no namespace entry", func(t *testing.T) {
		// A /proc whose own entry cannot be read says nothing of where
		// keploy runs: nothing is read by a kernel pid.
		root := fakeProcRoot(t, hostPIDNS, kernelPid, "sqs", "./sqs")
		if err := os.Remove(filepath.Join(root, "self", "ns", "pid")); err != nil {
			t.Fatal(err)
		}
		if src := (New(sysProc{root: root}, 0)).SourceOf(kernelPid); src == nil || src.Comm != "" || src.Unnamed != models.UnnamedPIDNamespace {
			t.Fatalf("SourceOf = %+v; want no name", src)
		}
	})
}

// The real /proc's answer is the namespace this test runs in: the kernel's
// own entry says so (utils.KernelPidsLocalIn).
func TestSysProcSaysWhereThisProcessRuns(t *testing.T) {
	link, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Skipf("no /proc/self/ns/pid: %v", err)
	}
	if got, want := (sysProc{}).KernelPidsLocal(), link == hostPIDNS; got != want {
		t.Fatalf("KernelPidsLocal = %v in %s; want %v", got, link, want)
	}
}
