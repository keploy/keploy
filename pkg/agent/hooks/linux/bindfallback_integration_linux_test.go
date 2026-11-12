//go:build linux && pidmap_integration

// Root-only, like the other pidmap_integration tests: it loads keploy's BPF
// object, attaches its bind hooks to a cgroup of the test's own, and sets the
// ephemeral range of a network namespace of its own.

package linux

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// bindMoveHelperEnv makes a re-executed test binary an app process
// (TestMain): with "tcp4:PORT" or "tcp6:PORT" it binds a TCP socket to
// PORT on 127.0.0.1 or ::1, says "bound N" with the port it got, and lives
// until its stdin closes.
const bindMoveHelperEnv = "KEPLOY_BINDMOVE_TEST_HELPER"

// ownPIDNetNSEnv marks the run inOwnPIDAndNetNamespaces re-executed.
const ownPIDNetNSEnv = "KEPLOY_BINDMOVE_TEST_NS"

func runBindMoveHelper(arg string) int {
	fail := func(err error) int { fmt.Println("helper:", err); return 3 }
	family, port, _ := strings.Cut(arg, ":")
	p, err := strconv.Atoi(port)
	if err != nil {
		return fail(err)
	}
	domain, sa := unix.AF_INET, unix.Sockaddr(&unix.SockaddrInet4{Port: p, Addr: [4]byte{127, 0, 0, 1}})
	if family == "tcp6" {
		domain, sa = unix.AF_INET6, &unix.SockaddrInet6{Port: p, Addr: [16]byte{15: 1}}
	}
	fd, err := unix.Socket(domain, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	if err := unix.Bind(fd, sa); err != nil {
		return fail(err)
	}
	got, err := unix.Getsockname(fd)
	if err != nil {
		return fail(err)
	}
	switch a := got.(type) {
	case *unix.SockaddrInet4:
		fmt.Println("bound", a.Port)
	case *unix.SockaddrInet6:
		fmt.Println("bound", a.Port)
	default:
		return fail(fmt.Errorf("bound to %T", got))
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = unix.Close(fd)
	return 0
}

// inOwnPIDAndNetNamespaces runs the calling test again, alone, as the first
// process of a new PID namespace (every process there that calls socket()
// is a member, as in inOwnPIDNamespace) in a new network namespace with its
// loopback up (whose sysctls it may set), and reports whether the caller is
// that run.
func inOwnPIDAndNetNamespaces(t *testing.T) bool {
	t.Helper()
	if os.Getenv(ownPIDNetNSEnv) == "1" {
		if err := loopbackUp(); err != nil {
			t.Fatalf("bring the namespace's loopback up: %v", err)
		}
		return true
	}
	if os.Geteuid() != 0 {
		t.Fatal("this test needs root (it loads BPF programs and creates namespaces); run the test binary with sudo")
	}
	cmd := exec.Command("/proc/self/exe", "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), ownPIDNetNSEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNET, Pdeathsig: syscall.SIGKILL}
	out, err := cmd.CombinedOutput()
	t.Logf("the run in PID and network namespaces of its own:\n%s", out)
	if err != nil {
		t.Fatalf("failed in PID and network namespaces of its own: %v", err)
	}
	if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("the run in PID and network namespaces of its own did not report %s as passed", t.Name())
	}
	return false
}

// bindMoveHooks loads keploy's main object as the agent does, attaches its
// bind hooks to a cgroup of the test's own with attachIngress, the
// post_bind4 hook refused, and registers this process as the agent, with the
// flags the agent's load gives them. It returns the hooks, the cgroup and
// the flags.
func bindMoveHooks(t *testing.T, logger *zap.Logger) (*Hooks, string, uint32) {
	t.Helper()
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	spec, err := hookSpec()
	if err != nil {
		t.Fatal(err)
	}
	h := NewHooks(logger, &config.Config{})
	if err := spec.LoadAndAssign(&h.objects, hookLoadOptions()); err != nil {
		t.Fatalf("load keploy's BPF object: %v", err)
	}
	t.Cleanup(func() { _ = h.objects.Close() })
	h.agentRegistrationMap = h.objects.KeployAgentRegistrationMap
	sock, err := link.Tracepoint("syscalls", "sys_enter_socket", h.objects.SyscallProbeEntrySocket, nil)
	if err != nil {
		t.Fatalf("attach sys_enter_socket: %v", err)
	}
	t.Cleanup(func() { _ = sock.Close() })
	cg, err := os.MkdirTemp(cgroupV2Root(t), "keploy-bindmove-test-")
	if err != nil {
		t.Fatalf("create a cgroup: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cg) })

	refused := errors.New("refused by the test")
	attach := func(o link.CgroupOptions) (link.Link, error) {
		if o.Attach == ebpf.AttachCGroupInet4PostBind {
			return nil, refused
		}
		return link.AttachCgroup(o)
	}
	if err := h.attachIngress(cg, h.objects, attach); err != nil {
		t.Fatalf("attach the bind hooks: %v", err)
	}
	t.Cleanup(func() {
		for _, l := range []link.Link{h.cgBind4, h.cgBind6, h.cgPostBind4, h.cgPostBind6} {
			if l != nil {
				_ = l.Close()
			}
		}
	})
	if h.kernelPortAlloc || h.postBindRefused == nil || !errors.Is(h.postBindRefused.err, refused) {
		t.Fatalf("post_bind4 refused: the kernel picks %v, refusal %+v; want the hooks to pick, the refusal kept", h.kernelPortAlloc, h.postBindRefused)
	}

	// The flags as the agent's load sets them: FlagKernelPortAlloc only
	// when both post_bind hooks attached.
	var info structs.AgentInfo
	if h.kernelPortAlloc {
		info.Flags |= structs.FlagKernelPortAlloc
	}
	info.KeployAgentNsPid = uint32(os.Getpid())
	if info.KeployAgentInode, info.KeployAgentDev, err = GetSelfInodeNumber(); err != nil {
		t.Fatal(err)
	}
	info.IsDocker = 1
	info.DNSPort = -1
	if err := h.SendAgentInfo(info); err != nil {
		t.Fatal(err)
	}
	client := structs.ClientInfo{Mode: 1, ClientNSPID: 0x7ffffff0}
	for i := range client.PassThroughPorts {
		client.PassThroughPorts[i] = -1
	}
	if err := h.objects.KeployClientRegistrationMap.Update(uint64(0), client, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0) // registers this process as the agent
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)
	return h, cg, info.Flags
}

// boundInCgroup runs the bind-move helper with arg in cg and returns the
// port its bind got.
func boundInCgroup(t *testing.T, cg, arg string) int {
	t.Helper()
	cgf, err := os.Open(cg)
	if err != nil {
		t.Fatal(err)
	}
	defer cgf.Close()
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	cmd.Env = append(os.Environ(), bindMoveHelperEnv+"="+arg)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgf.Fd()), Pdeathsig: syscall.SIGKILL}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	var port int
	if n, _ := fmt.Sscanf(line, "bound %d", &port); n != 1 {
		t.Fatalf("the bind-move helper (%s) said %q (%v), want bound PORT", arg, line, err)
	}
	return port
}

// With a post_bind hook refused, keploy's bind hooks pick the port an app's
// bind moves to themselves (keploy/ebpf's find_free_port), and the warning
// keploy gives says where from its Go mirror of that choice
// (readBindMovePorts): the bind lands in the ports the warning names, and,
// when it names none, stays on the port the app asked for with nothing
// noted for it. A later copy of the hooks' objects that changes their
// choice fails here, in a network namespace whose ephemeral range and
// unprivileged start the test sets.
func TestTheBindHooksMoveABindWhereTheWarningSays(t *testing.T) {
	for _, r := range []struct {
		name, rng, unpriv string // unpriv "" leaves it as it is (1024)
		port              int    // the port the app asks for
		first, last       int    // the ports it may move to; 0, 0 for none
	}{
		{"the default range", "32768 60999", "", 8080, 61000, 65535},
		{"range from 20000, unprivileged from 19999", "20000 65535", "19999", 40000, 19999, 19999},
		{"range from 1024", "1024 65535", "", 32767, 0, 0},
	} {
		for _, family := range []string{"tcp4", "tcp6"} {
			t.Run(r.name+", "+family, func(t *testing.T) {
				if !inOwnPIDAndNetNamespaces(t) {
					return
				}
				// The kernel refuses an unprivileged start past the range's
				// start, so the range is set first.
				if err := os.WriteFile("/proc/sys/net/ipv4/ip_local_port_range", []byte(r.rng), 0); err != nil {
					t.Fatal(err)
				}
				if r.unpriv != "" {
					if err := os.WriteFile("/proc/sys/net/ipv4/ip_unprivileged_port_start", []byte(r.unpriv), 0); err != nil {
						t.Fatal(err)
					}
				}
				p, err := readBindMovePorts(netSysctlDir)
				if err != nil {
					t.Fatal(err)
				}
				if p.first != r.first || p.last != r.last {
					t.Fatalf("readBindMovePorts with range %q and unprivileged start %d: %d-%d, want %d-%d", r.rng, p.unpriv, p.first, p.last, r.first, r.last)
				}

				core, logs := observer.New(zapcore.WarnLevel)
				h, cg, flags := bindMoveHooks(t, zap.New(core))
				h.sayBindFallback(flags)
				all := logs.All()
				if len(all) != 1 {
					t.Fatalf("%d warnings, want 1: %v", len(all), all)
				}
				msg := all[0].Message
				said := "no port is left"
				if r.first != 0 {
					said = fmt.Sprintf("a port in %d-%d", r.first, r.last)
				}
				if !strings.Contains(msg, said) {
					t.Fatalf("the warning %q does not say %q", msg, said)
				}

				moved := boundInCgroup(t, cg, fmt.Sprintf("%s:%d", family, r.port))
				t.Logf("the bind of %d got %d; the warning: %s", r.port, moved, msg)
				var v appPortTranslate
				lookupErr := h.objects.AppPortInfo.Lookup(uint16(r.port), &v)
				if r.first == 0 {
					if moved != r.port {
						t.Fatalf("the bind of %d got %d, where the warning says it is left alone", r.port, moved)
					}
					if !errors.Is(lookupErr, ebpf.ErrKeyNotExist) {
						t.Fatalf("app_port_info[%d] = %+v (%v), want no entry for a bind left alone", r.port, v, lookupErr)
					}
					return
				}
				if moved < p.first || moved > p.last {
					t.Fatalf("the bind of %d was moved to %d, outside %d-%d, where the warning says it goes", r.port, moved, p.first, p.last)
				}
				if lookupErr != nil || v.ProxyPort != uint16(r.port) || int(v.ApplicationPort) != moved {
					t.Fatalf("app_port_info[%d] = %+v (%v), want the bind's move to %d", r.port, v, lookupErr, moved)
				}
			})
		}
	}
}
