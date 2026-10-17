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
	var k uint64
	if h.objects.ReadySeen.Iterate().Next(&k, new(uint8)) {
		t.Fatalf("after the mark ready_seen still holds %#x", k)
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
