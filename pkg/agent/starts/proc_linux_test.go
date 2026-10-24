//go:build linux

package starts

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
		// Said once per recording: a reset (the next record session) says
		// it again, once.
		r.NoteOrigin("8", kernelPid)
		r.StampOrigin(&models.Mock{Name: "mock-7", ConnectionID: "8", Origin: &models.Origin{Clock: "proxy:run-1"}})
		if n := logs.FilterMessageSnippet("keploy runs in a PID namespace of its own").Len(); n != 1 {
			t.Fatalf("said %d times before a reset; want once", n)
		}
		r.Reset()
		r.NoteOrigin("9", kernelPid)
		r.StampOrigin(&models.Mock{Name: "mock-0", ConnectionID: "9", Origin: &models.Origin{Clock: "proxy:run-2"}})
		if n := logs.FilterMessageSnippet("keploy runs in a PID namespace of its own").Len(); n != 2 {
			t.Fatalf("said %d times after a reset; want twice", n)
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
	// This process's /proc, by either name, is answered by what this
	// process's own entry says (ownPidsLocal), each way; another root by
	// its own entry.
	defer func(f func() bool) { ownPidsLocal = f }(ownPidsLocal)
	private := fakeProcRoot(t, privatePIDNS, 1, "sh", "/bin/sh")
	for _, own := range []bool{false, true} {
		ownPidsLocal = func() bool { return own }
		for _, p := range []sysProc{{}, {root: "/proc"}} {
			if got := p.KernelPidsLocal(); got != own {
				t.Fatalf("%+v: KernelPidsLocal = %v where this process's entry says %v", p, got, own)
			}
		}
		if (sysProc{root: private}).KernelPidsLocal() {
			t.Fatal("a root whose own entry is a private namespace's read as the kernel's")
		}
	}
}

// hostLocal is this host's /proc, read by the pids of this test's own PID
// namespace: a process the test starts tells its pid in that namespace.
type hostLocal struct{ sysProc }

func (hostLocal) KernelPidsLocal() bool { return true }

// A program a real go run built runs from the go tool's build dir for that
// one run, or from the go build cache once built: its argv[0] is a path that
// differs from one run to the next. keploy names it by its go run, `go run .`,
// the same for two runs, its build flags left out.
func TestAGoRunProgramIsNamedByItsGoRunOnThisHost(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH to run a program with")
	}
	dir := t.TempDir()
	for name, text := range map[string]string{
		"go.mod":  "module example.com/orders\n\ngo 1.21\n",
		"main.go": "package main\n\nimport (\n\t\"fmt\"\n\t\"io\"\n\t\"os\"\n)\n\nvar key string\n\nfunc main() {\n\tfmt.Println(os.Getpid())\n\t_, _ = io.Copy(io.Discard, os.Stdin)\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func() (argv0, exe string) {
		cmd := exec.Command(gobin, "run", "-ldflags=-X=main.key=s3cret", ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=", "GOTOOLCHAIN=local", "GOWORK=off", "GOPROXY=off")
		cmd.Stderr = os.Stderr
		// The go tool and the program it runs are one process group,
		// killed whole however the test ends.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		})
		line, err := bufio.NewReader(out).ReadString('\n')
		if err != nil {
			t.Fatalf("the go run's program said no pid: %v", err)
		}
		pid, err := strconv.ParseUint(strings.TrimSpace(line), 10, 32)
		if err != nil {
			t.Fatalf("the go run's program said %q", line)
		}
		_, argv, _ := (sysProc{}).Command(uint32(pid))
		if len(argv) > 0 {
			argv0 = argv[0]
		}
		_, exe, ok := New(hostLocal{}, 0).ProcessOf(uint32(pid))
		if !ok {
			t.Fatalf("pid %d could not be read", pid)
		}
		_ = in.Close()
		if err := cmd.Wait(); err != nil {
			t.Fatalf("the go run ended with %v", err)
		}
		return argv0, exe
	}
	firstArgv0, first := run()
	secondArgv0, second := run()
	if first != "go run ." || second != first {
		t.Fatalf("two go runs of the app are named %q and %q; want both %q (their argv[0]s: %q, %q)", first, second, "go run .", firstArgv0, secondArgv0)
	}
}

// A test binary a real go test built runs from the go tool's work dir for
// that one go test: its argv[0] is a path that differs from one run to the
// next. keploy names it by its go test and its packages, `go test ./`, the
// same for two runs, its build and test flags left out.
func TestAGoTestBinaryIsNamedByItsGoTestOnThisHost(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH to run a test with")
	}
	dir := t.TempDir()
	for name, text := range map[string]string{
		"go.mod": "module example.com/store\n\ngo 1.21\n",
		// The test says its pid and runs until it is told to end (a file
		// named done), so keploy reads it while it runs.
		"store_test.go": "package store\n\nimport (\n\t\"os\"\n\t\"path/filepath\"\n\t\"strconv\"\n\t\"testing\"\n\t\"time\"\n)\n\n" +
			"func TestHold(t *testing.T) {\n\tdir := os.Getenv(\"HOLD_DIR\")\n" +
			"\tif err := os.WriteFile(filepath.Join(dir, \"pid.tmp\"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {\n\t\tt.Fatal(err)\n\t}\n" +
			"\tif err := os.Rename(filepath.Join(dir, \"pid.tmp\"), filepath.Join(dir, \"pid\")); err != nil {\n\t\tt.Fatal(err)\n\t}\n" +
			"\tfor {\n\t\tif _, err := os.Stat(filepath.Join(dir, \"done\")); err == nil {\n\t\t\treturn\n\t\t}\n\t\ttime.Sleep(10 * time.Millisecond)\n\t}\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(n int) (argv0, exe string) {
		hold := t.TempDir()
		cmd := exec.Command(gobin, "test", "-count=1", "-v", "-ldflags=-X=main.key=s3cret", "-run", "TestHold", "./")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=", "GOTOOLCHAIN=local", "GOWORK=off", "GOPROXY=off", "HOLD_DIR="+hold)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		// The go tool and the test binary it runs are one process group,
		// killed whole however the test ends.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ended := make(chan error, 1)
		go func() { ended <- cmd.Wait() }()
		t.Cleanup(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-ended
			ended <- nil
		})
		// Wait for the test binary's pid, or for the go test to end
		// without one: no wall-clock bound.
		var raw []byte
		for {
			if raw, err = os.ReadFile(filepath.Join(hold, "pid")); err == nil {
				break
			}
			select {
			case err := <-ended:
				ended <- err
				t.Fatalf("go test %d ended before its test said its pid: %v", n, err)
			case <-time.After(10 * time.Millisecond):
			}
		}
		pid, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 32)
		if err != nil {
			t.Fatalf("the test binary said %q", raw)
		}
		_, argv, _ := (sysProc{}).Command(uint32(pid))
		if len(argv) > 0 {
			argv0 = argv[0]
		}
		_, exe, ok := New(hostLocal{}, 0).ProcessOf(uint32(pid))
		if !ok {
			t.Fatalf("pid %d could not be read", pid)
		}
		if err := os.WriteFile(filepath.Join(hold, "done"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := <-ended; err != nil {
			t.Fatalf("go test %d ended with %v", n, err)
		}
		ended <- nil
		return argv0, exe
	}
	firstArgv0, first := run(1)
	secondArgv0, second := run(2)
	if first != "go test ./" || second != first {
		t.Fatalf("two go tests' binaries are named %q and %q; want both %q (their argv[0]s: %q, %q)", first, second, "go test ./", firstArgv0, secondArgv0)
	}
	if firstArgv0 == secondArgv0 {
		t.Logf("the two test binaries ran from one path, %q: the row does not show the work dir changing", firstArgv0)
	}
}
