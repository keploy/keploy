//go:build linux && pidmap_integration

package linux

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// readyHelperEnv makes a re-executed test binary one of the readiness
// tests' applications (runReadyHelper).
const readyHelperEnv = "KEPLOY_READY_TEST_HELPER"

// burn uses about d of CPU time.
func burn(d time.Duration) {
	var ru syscall.Rusage
	used := func() time.Duration {
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
		return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	sum := sha256.Sum256(nil)
	for start := used(); used()-start < d; {
		for i := 0; i < 2000; i++ {
			sum = sha256.Sum256(sum[:])
		}
	}
}

// runReadyHelper is an application under a readiness test:
//
//	listen-after:MS  uses MS ms of CPU, then listens on 127.0.0.1 (an
//	                 ephemeral port): "ready PORT"
//	cpu-child        waits for a line, calls socket() (the keploy client
//	                 registers so), starts a child of its own that only uses CPU,
//	                 and says "ready"
//	cpu              only uses CPU, for ever
//	sleeper          waits for a line, calls socket() (registers as the
//	                 client), says "ready"; then at each "run [NAME]" line
//	                 starts a child (sleeper-child: this binary, another
//	                 command line than its own, one for every NAME) and says
//	                 "ready" once the child says "up", and at each
//	                 "kill [NAME]" kills that child and waits for it (a
//	                 NAME it has no child for fails)
//	sleeper-child    says "up": its exec, and the exec's tracepoint, are done
//	exec-twice       waits for a line, calls socket() (registers as the
//	                 client), runs /bin/sh -c "exit 0" twice, and says "ready"
//
// each until its stdin closes.
func runReadyHelper(mode string) int {
	fail := func(err error) int { fmt.Println("helper:", err); return 3 }
	switch {
	case strings.HasPrefix(mode, "listen-after:"):
		ms, _ := strconv.Atoi(strings.TrimPrefix(mode, "listen-after:"))
		burn(time.Duration(ms) * time.Millisecond)
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err != nil {
			return fail(err)
		}
		if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
			return fail(err)
		}
		if err := unix.Listen(fd, 1); err != nil {
			return fail(err)
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			return fail(err)
		}
		fmt.Println("ready", sa.(*unix.SockaddrInet4).Port)
	case mode == "cpu-child":
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			return fail(err)
		}
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
		if err != nil {
			return fail(err)
		}
		_ = unix.Close(fd)
		child := exec.Command("/proc/self/exe", "-test.run=^$")
		child.Env = append(os.Environ(), readyHelperEnv+"=cpu")
		child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := child.Start(); err != nil {
			return fail(err)
		}
		fmt.Println("ready")
	case mode == "cpu":
		for {
			burn(time.Second)
		}
	case mode == "sleeper-child":
		fmt.Println("up")
	case mode == "sleeper":
		in := bufio.NewReader(os.Stdin)
		if _, err := in.ReadString('\n'); err != nil {
			return fail(err)
		}
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
		if err != nil {
			return fail(err)
		}
		_ = unix.Close(fd)
		fmt.Println("ready")
		children := map[string]*exec.Cmd{}
		for {
			line, err := in.ReadString('\n')
			if err != nil {
				return 0
			}
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			name := strings.Join(f[1:], " ")
			switch f[0] {
			case "run":
				// Start returns once the exec closed the child's
				// close-on-exec descriptors, before the exec is done and its
				// tracepoint has run; the child's "up" comes after both.
				child := exec.Command("/proc/self/exe", "-test.run=^$", "-test.count=2")
				child.Env = append(os.Environ(), readyHelperEnv+"=sleeper-child")
				child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
				if _, err := child.StdinPipe(); err != nil {
					return fail(err)
				}
				up, err := child.StdoutPipe()
				if err != nil {
					return fail(err)
				}
				if err := child.Start(); err != nil {
					return fail(err)
				}
				if line, _ := bufio.NewReader(up).ReadString('\n'); strings.TrimSpace(line) != "up" {
					return fail(fmt.Errorf("the child: %q", line))
				}
				children[name] = child
			case "kill":
				// Wait returns once the child was reaped, after its exit
				// tracepoint.
				child := children[name]
				if child == nil {
					return fail(fmt.Errorf("kill %q: no such child", name))
				}
				_ = child.Process.Kill()
				_ = child.Wait()
				delete(children, name)
			}
			fmt.Println("ready")
		}
	case mode == "exec-twice":
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			return fail(err)
		}
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
		if err != nil {
			return fail(err)
		}
		_ = unix.Close(fd)
		for i := 0; i < 2; i++ {
			if err := exec.Command("/bin/sh", "-c", "exit 0").Run(); err != nil {
				return fail(err)
			}
		}
		fmt.Println("ready")
	default:
		return fail(fmt.Errorf("unknown mode %q", mode))
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// provenReadyHooks loads keploy's object, attaches what the hooks' load does
// for the readiness watch to a cgroup of the test's own (k_sockops and the
// bind programs; the fork and exec tracepoints), sets the agent's flags, and
// returns the hooks and the cgroup.
func provenReadyHooks(t *testing.T, docker bool) (*Hooks, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("this test needs root (it loads BPF programs); run the test binary with sudo")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(provenswitch.Env, "1")
	spec, err := hookSpec()
	if err != nil {
		t.Fatal(err)
	}
	h := NewHooks(zap.NewNop(), &config.Config{})
	if err := spec.LoadAndAssign(&h.objects, nil); err != nil {
		t.Fatalf("load keploy's BPF object: %v", err)
	}
	t.Cleanup(func() { _ = h.objects.Close() })
	h.agentRegistrationMap = h.objects.KeployAgentRegistrationMap
	sock, err := link.Tracepoint("syscalls", "sys_enter_socket", h.objects.SyscallProbeEntrySocket, nil)
	if err != nil {
		t.Fatalf("attach sys_enter_socket: %v", err)
	}
	t.Cleanup(func() { _ = sock.Close() })
	cg, err := os.MkdirTemp(cgroupV2Root(t), "keploy-ready-test-")
	if err != nil {
		t.Fatalf("create a cgroup: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cg) })
	l, err := link.AttachCgroup(link.CgroupOptions{Path: cg, Attach: ebpf.AttachCGroupSockOps, Program: h.objects.K_sockops})
	if err != nil {
		t.Fatalf("attach k_sockops: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	flags, err := h.startProvenReady(cg, docker, models.MODE_TEST)
	if err != nil || flags&structs.FlagProvenReady == 0 {
		t.Fatalf("startProvenReady under the switch = %#x, %v; want FlagProvenReady", flags, err)
	}
	t.Cleanup(func() { h.ready.close() })
	// The agent's own bind records its mount namespace (registerSelf): keploy
	// attaches the bind programs at the root, where this process is too.
	for _, p := range []struct {
		prog   *ebpf.Program
		attach ebpf.AttachType
	}{{h.objects.K_readyBind4, ebpf.AttachCGroupInet4Bind}, {h.objects.K_readyBind6, ebpf.AttachCGroupInet6Bind}} {
		l, err := link.AttachCgroup(link.CgroupOptions{Path: cgroupV2Root(t), Attach: p.attach, Program: p.prog})
		if err != nil {
			t.Fatalf("attach a bind program at the root: %v", err)
		}
		t.Cleanup(func() { _ = l.Close() })
	}
	var info structs.AgentInfo
	info.KeployAgentNsPid = uint32(os.Getpid())
	if info.KeployAgentInode, info.KeployAgentDev, err = GetSelfInodeNumber(); err != nil {
		t.Fatal(err)
	}
	if docker {
		info.IsDocker = 1
	}
	info.DNSPort = -1
	info.Flags = flags
	if err := h.SendAgentInfo(info); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0) // registers this process as the agent
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)
	// As the hooks' load does once the agent is registered, before any
	// application starts: in Docker mode the kernel records the agent's
	// container (registerSelf), until when nothing is a member.
	if err := h.ready.selfRegistered(); err != nil {
		t.Fatalf("record the agent's container: %v", err)
	}
	// The helpers are this test's children, and the kernel takes no
	// descendant of the agent's for a container's main (a real
	// application's main is its runtime's child): from here the agent is a
	// pid no process has.
	if err := h.objects.KeployAgentKernelPidMap.Update(uint32(0), uint32(0x7ffffff0), ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	return h, cg
}

// ownNetNSEnv marks the run inOwnNetNamespace started.
const ownNetNSEnv = "KEPLOY_READY_TEST_NETNS"

// inOwnNetNamespace runs the calling test again, alone, in a network
// namespace of its own with its loopback up, and reports whether the caller
// is that run. The agent refuses to watch a network namespace that a
// process of another mount namespace already runs in (strangerInNetns,
// read from its /proc), and the host's always holds some: the kdevtmpfs
// kernel thread on any kernel, and services with mounts of their own. A
// Docker-mode agent never meets them: its container has a PID namespace of
// its own, so its /proc lists none of the host's processes (even with
// --network host, pre-flip W6-u). The test's agent, the test process, runs
// in the host's PID namespace, so it gets a network namespace of its own,
// which none of them share.
func inOwnNetNamespace(t *testing.T) bool {
	t.Helper()
	if os.Getenv(ownNetNSEnv) == "1" {
		if err := loopbackUp(); err != nil {
			t.Fatalf("bring the namespace's loopback up: %v", err)
		}
		return true
	}
	if os.Geteuid() != 0 {
		t.Fatal("this test needs root (it loads BPF programs and creates a network namespace); run the test binary with sudo")
	}
	cmd := exec.Command("/proc/self/exe", "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), ownNetNSEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET, Pdeathsig: syscall.SIGKILL}
	out, err := cmd.CombinedOutput()
	t.Logf("the run in a network namespace of its own:\n%s", out)
	if err != nil {
		t.Fatalf("failed in a network namespace of its own: %v", err)
	}
	if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("the run in a network namespace of its own did not report %s as passed", t.Name())
	}
	return false
}

// loopbackUp sets this network namespace's lo up (a new namespace's is down,
// with no address to bind).
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

func startReadyHelper(t *testing.T, cg, mode string) (io.Writer, *bufio.Reader, int) {
	t.Helper()
	cgf, err := os.Open(cg)
	if err != nil {
		t.Fatal(err)
	}
	defer cgf.Close()
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	cmd.Env = append(os.Environ(), readyHelperEnv+"="+mode)
	// In a mount namespace of its own, as an application's container is: in
	// Docker mode one in the agent's (this test's) is the agent's.
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgf.Fd()), Pdeathsig: syscall.SIGKILL, Cloneflags: syscall.CLONE_NEWNS}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return stdin, bufio.NewReader(stdout), cmd.Process.Pid
}

// readiness reads the hooks' AppReadiness until done says so; its report of
// a listen comes through the ring, read by the watch's goroutine.
func readiness(t *testing.T, h *Hooks, done func(models.AppReadiness) bool) models.AppReadiness {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		st, err := h.AppReadiness(0)
		if err != nil {
			t.Fatal(err)
		}
		if done(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("readiness never got there: %+v", st)
		}
	}
}

// Under the switch the agent sees the application listen, in its own network
// namespace, and the listen is its progress.
func TestUnderTheSwitchTheAgentSeesTheApplicationListen(t *testing.T) {
	if !inOwnNetNamespace(t) {
		return
	}
	h, cg := provenReadyHooks(t, true)
	in, out, _ := startReadyHelper(t, cg, "listen-after:100")
	line, _ := out.ReadString('\n')
	port, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "ready "))
	if err != nil {
		t.Fatalf("helper: %q", line)
	}
	st := readiness(t, h, func(st models.AppReadiness) bool { return len(st.Listens) > 0 })
	want := models.AppListen{Port: uint16(port), Addr: "127.0.0.1", Here: true, Live: true}
	got := st.Listens[0]
	got.TGID, got.Cookie = 0, 0
	if got != want || st.Listens[0].Cookie == 0 || st.Progress.Listens < 1 || !st.CPUKnown {
		t.Fatalf("readiness %+v; want the listen %+v, live, counted, and the CPU time known", st, want)
	}
	// The agent's own bind recorded its container: this test's mount
	// namespace, the helper's a container of its own whose main it is.
	var st2 syscall.Stat_t
	if err := syscall.Stat("/proc/thread-self/ns/mnt", &st2); err != nil {
		t.Fatal(err)
	}
	var mnt uint32
	if err := h.objects.ReadyAgentMntns.Lookup(uint32(0), &mnt); err != nil || mnt != uint32(st2.Ino) {
		t.Fatalf("ready_agent_mntns %d (%v), want this test's mount namespace %d", mnt, err, st2.Ino)
	}
	// The counts a set's listens begin after: from ListenCount none, from
	// one before the listen's own.
	if st.ListenCount < 1 {
		t.Fatalf("readiness %+v: want the listen counted", st)
	}
	none, err := h.AppReadiness(st.ListenCount)
	if err != nil || len(none.Listens) != 0 || none.ListenCount != st.ListenCount {
		t.Fatalf("from %d: %+v (%v), want no listen, the same count", st.ListenCount, none, err)
	}
	one, err := h.AppReadiness(st.ListenCount - 1)
	if err != nil || len(one.Listens) != 1 || one.Listens[0].Seq != st.ListenCount-1 {
		t.Fatalf("from %d: %+v (%v), want the one listen, Seq %d", st.ListenCount-1, one, err, st.ListenCount-1)
	}
	// The set's mark answers the same count, and forgets what the set's
	// processes did.
	marked, err := h.MarkReadiness()
	if err != nil || marked.ListenCount != st.ListenCount || len(marked.Listens) != 0 {
		t.Fatalf("mark: %+v (%v), want the count %d, no listen", marked, err, st.ListenCount)
	}
	for _, m := range []*ebpf.Map{h.objects.ReadySeen, h.objects.ReadyHolders} {
		k, v := make([]byte, m.KeySize()), make([]byte, m.ValueSize())
		it := m.Iterate()
		if it.Next(k, v) {
			t.Fatalf("after the mark %s still holds %x", m, k)
		}
		if err := it.Err(); err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
	}
	// The helper ends, its socket with it: the listen is told, no longer live.
	if c, ok := in.(io.Closer); ok {
		_ = c.Close()
	}
	closed := readiness(t, h, func(st models.AppReadiness) bool { return len(st.Listens) > 0 && !st.Listens[0].Live })
	if closed.Listens[0].Port != uint16(port) {
		t.Fatalf("readiness %+v; want the listen on :%d, no longer live", closed, port)
	}
}

// A start that only computes, as a native app's grandchild: its CPU time
// moves, though it listens, connects, forks and execs nothing.
func TestUnderTheSwitchACPUOnlyStartMovesTheApplicationsCPUTime(t *testing.T) {
	h, cg := provenReadyHooks(t, false)
	in, out, pid := startReadyHelper(t, cg, "cpu-child")
	client := structs.ClientInfo{Mode: 1, ClientNSPID: uint32(pid)}
	for i := range client.PassThroughPorts {
		client.PassThroughPorts[i] = -1
	}
	if err := h.objects.KeployClientRegistrationMap.Update(uint64(0), client, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	if line, _ := out.ReadString('\n'); strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper: %q", line)
	}
	var registered uint32
	if err := h.objects.KeployClientKernelPidMap.Lookup(uint32(0), &registered); err != nil || registered == 0 {
		t.Fatalf("the helper did not register as the keploy client: %d, %v", registered, err)
	}
	first := readiness(t, h, func(st models.AppReadiness) bool { return st.CPUKnown })
	moved := readiness(t, h, func(st models.AppReadiness) bool { return st.CPU > first.CPU })
	if len(moved.Listens) != 0 {
		t.Fatalf("readiness %+v: the CPU-only start listens nowhere", moved)
	}
	t.Logf("CPU %v -> %v, progress %+v", first.CPU, moved.CPU, moved.Progress)
}

// A native app that runs one command line twice restarted once: the agent
// answers the kernel's count of restarts and names the program, and its
// mark answers the count the set's restarts begin at.
func TestUnderTheSwitchTheAgentCountsARestartAndNamesIt(t *testing.T) {
	h, cg := provenReadyHooks(t, false)
	in, out, pid := startReadyHelper(t, cg, "exec-twice")
	client := structs.ClientInfo{Mode: 1, ClientNSPID: uint32(pid)}
	for i := range client.PassThroughPorts {
		client.PassThroughPorts[i] = -1
	}
	if err := h.objects.KeployClientRegistrationMap.Update(uint64(0), client, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	if line, _ := out.ReadString('\n'); strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper: %q", line)
	}
	st := readiness(t, h, func(st models.AppReadiness) bool { return st.Progress.Restarts >= 1 })
	if st.Progress.Restarts != 1 || st.LastRestart != "/bin/sh" || st.Progress.Execs < 1 {
		t.Fatalf("readiness %+v: want one restart, of /bin/sh, after its first exec", st)
	}
	marked, err := h.MarkReadiness()
	if err != nil || marked.Progress.Restarts != st.Progress.Restarts {
		t.Fatalf("mark: %+v (%v), want the restarts so far, %d", marked.Progress, err, st.Progress.Restarts)
	}
}

// A run that starts while the set's mark clears (its exec between the two
// clears) is let go of at its exit: its command line is then new to the set,
// and run again a restart. Cleared the other way round (ready_seen first),
// the run's live count outlived the holder the mark had cleared, and every
// later run of the command line was a repeat, never a restart.
func TestARunTheMarkClearsAroundIsLetGoOfAtItsExit(t *testing.T) {
	h, cg := provenReadyHooks(t, false)
	in, out, pid := startReadyHelper(t, cg, "sleeper")
	client := structs.ClientInfo{Mode: 1, ClientNSPID: uint32(pid)}
	for i := range client.PassThroughPorts {
		client.PassThroughPorts[i] = -1
	}
	if err := h.objects.KeployClientRegistrationMap.Update(uint64(0), client, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	step := func(line string) {
		t.Helper()
		if _, err := in.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		if got, _ := out.ReadString('\n'); strings.TrimSpace(got) != "ready" {
			t.Fatalf("helper, after %q: %q", line, got)
		}
	}
	progress := func() models.ReadyProgress {
		t.Helper()
		st, err := h.AppReadiness(0)
		if err != nil {
			t.Fatal(err)
		}
		return st.Progress
	}
	step("go")
	markBetweenClears = func() { step("run") }
	t.Cleanup(func() { markBetweenClears = nil })
	if _, err := h.MarkReadiness(); err != nil {
		t.Fatalf("mark: %v", err)
	}
	markBetweenClears = nil
	step("kill")
	// Counted in the exec's own tracepoint, before the child said "up".
	before := progress()
	step("run")
	if got := progress(); got.Execs != before.Execs+1 || got.Restarts != before.Restarts {
		t.Fatalf("the command line run again after the mark: progress %+v -> %+v, want one exec more (new to the set)", before, got)
	}
	step("kill")
	before = progress()
	step("run")
	if got := progress(); got.Restarts != before.Restarts+1 {
		t.Fatalf("the command line run again once its last run exited: progress %+v -> %+v, want one restart more", before, got)
	}
	step("kill")
}

// A known limit of the mark, pre-flip W6-ready-holders-generation: a run
// exec'd between its two clears stays its command line's holder after the
// mark, while ready_seen forgot its live run. Its exit, or its exec of
// another command line, after a fresh run of the command line has started,
// lets go of the fresh run's live count; the count then stays one short
// until every run of the command line has exited (ready_release stops at 0),
// and until then a run beside a single live run is counted as a restart, a
// false one (a second worker), each time. That is a start
// not judged (its CPU time no longer weighed), never a false pass. A release
// checked against the run it counted closes it (design in the ledger: an
// instance stamp each ready_seen entry takes at insert and its holders copy,
// or the mark as a generation ebpf reads at each exec); a stamp of the
// mark's generation on holders alone does not. This pins today's behaviour,
// so the fix shows here; the control, a run started before the mark, is let
// go of at the mark and counts nothing.
func TestAWindowRunOutlivingAFreshRunIsAFalseRestartToday(t *testing.T) {
	for _, inWindow := range []bool{true, false} {
		t.Run(fmt.Sprintf("run 0 between the clears: %v", inWindow), func(t *testing.T) {
			h, cg := provenReadyHooks(t, false)
			in, out, pid := startReadyHelper(t, cg, "sleeper")
			client := structs.ClientInfo{Mode: 1, ClientNSPID: uint32(pid)}
			for i := range client.PassThroughPorts {
				client.PassThroughPorts[i] = -1
			}
			if err := h.objects.KeployClientRegistrationMap.Update(uint64(0), client, ebpf.UpdateAny); err != nil {
				t.Fatal(err)
			}
			step := func(line string) {
				t.Helper()
				if _, err := in.Write([]byte(line + "\n")); err != nil {
					t.Fatal(err)
				}
				if got, _ := out.ReadString('\n'); strings.TrimSpace(got) != "ready" {
					t.Fatalf("helper, after %q: %q", line, got)
				}
			}
			progress := func() models.ReadyProgress {
				t.Helper()
				st, err := h.AppReadiness(0)
				if err != nil {
					t.Fatal(err)
				}
				return st.Progress
			}
			step("go")
			if inWindow {
				markBetweenClears = func() { step("run 0") }
				t.Cleanup(func() { markBetweenClears = nil })
			} else {
				step("run 0")
			}
			if _, err := h.MarkReadiness(); err != nil {
				t.Fatalf("mark: %v", err)
			}
			markBetweenClears = nil
			step("run 1") // a fresh run, run 0 still running
			step("kill 0")
			before := progress()
			step("run 2") // run 1 still running: a second worker
			got := progress()
			want := before.Restarts
			if inWindow {
				want++ // today's false restart
			}
			if got.Restarts != want {
				t.Fatalf("run 2, beside run 1: restarts %d -> %d, want %d (in the window: %v)", before.Restarts, got.Restarts, want, inWindow)
			}
			step("kill 2")
			step("kill 1")
		})
	}
}

// The mark clears ready_holders while the kernel deletes from it (a member's
// exit), which aborts an iteration: clearMap reads again, so the mark does
// not fail and leave the set not judged. Here a full map is churned, its
// keys deleted and put back, as clearMap runs, twenty times.
func TestClearingAMapItsKeysChurnInGoesOn(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("this test needs root (it creates BPF maps); run the test binary with sudo")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	const n = 8192
	for round := 0; round < 20; round++ {
		m, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Hash, KeySize: 4, ValueSize: 8, MaxEntries: n})
		if err != nil {
			t.Fatal(err)
		}
		for k := uint32(0); k < n; k++ {
			if err := m.Put(k, uint64(k)); err != nil {
				t.Fatal(err)
			}
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for k := uint32(0); ; k = (k + 1) % n {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.Delete(k)
				_ = m.Put(k, uint64(k))
			}
		}()
		err = clearMap(m)
		close(stop)
		<-done
		_ = m.Close()
		if err != nil {
			t.Fatalf("round %d: clearMap of a map its keys churn in: %v", round, err)
		}
	}
}
