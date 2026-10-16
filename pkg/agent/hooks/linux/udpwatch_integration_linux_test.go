//go:build linux && pidmap_integration

// Root-only, like pidmaps_integration_linux_test.go: it loads keploy's BPF
// object and attaches cgroup programs.

package linux

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// runUDPServerHelper is the UDP helper as an application's server: bound on
// 127.0.0.1, it says its port, then at each line on stdin receives one
// request and answers it ("ready served"), until stdin closes.
func runUDPServerHelper() int {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err == nil {
		err = unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
	}
	var sa unix.Sockaddr
	if err == nil {
		sa, err = unix.Getsockname(fd)
	}
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	fmt.Println("ready", sa.(*unix.SockaddrInet4).Port)
	in := bufio.NewReader(os.Stdin)
	buf := make([]byte, 16)
	for {
		if _, err := in.ReadString('\n'); err != nil {
			return 0
		}
		_, from, err := unix.Recvfrom(fd, buf, 0)
		if err == nil {
			err = unix.Sendto(fd, []byte("pong"), 0, from)
		}
		if err != nil {
			fmt.Println("helper:", err)
			return 3
		}
		fmt.Println("ready served")
	}
}

// udpHelperEnv makes a re-executed test binary a UDP client: it connects a
// UDP socket to 127.0.0.1:PORT, sends, waits for the reply, tries UDP 443
// (a connect(), a datagram on it, an unconnected send), says what each did;
// then, at a line on stdin, sends and waits for the reply again on the
// first socket, and lives until its stdin closes.
const udpHelperEnv = "KEPLOY_UDP_TEST_HELPER"

func runUDPHelper(port string) int {
	if port == "server" {
		return runUDPServerHelper()
	}
	p, _ := strconv.Atoi(port)
	in := bufio.NewReader(os.Stdin)
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err == nil {
		err = unix.Connect(fd, &unix.SockaddrInet4{Port: p, Addr: [4]byte{127, 0, 0, 1}})
	}
	exchange := func() error {
		if _, err := unix.Write(fd, []byte("ping")); err != nil {
			return err
		}
		_, err := unix.Read(fd, make([]byte, 16))
		return err
	}
	if err == nil {
		err = exchange()
	}
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	quicAt := &unix.SockaddrInet4{Port: 443, Addr: [4]byte{127, 0, 0, 1}}
	conn443, _ := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	cerr := unix.Connect(conn443, quicAt)
	_, serr := unix.Write(conn443, []byte("q"))
	other, _ := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	terr := unix.Sendto(other, []byte("q"), 0, quicAt)
	fmt.Printf("ready connect443=%v send443=%v sendto443=%v\n", cerr, serr, terr)
	for {
		if _, err := in.ReadString('\n'); err != nil {
			return 0
		}
		if err := exchange(); err != nil {
			fmt.Println("helper:", err)
			return 3
		}
		fmt.Println("ready again")
	}
}

// Under the proven switch, keploy's own object: the application's UDP that a
// real server answers is a udp_reply capture loss in Hooks.CaptureLosses,
// counted again when the same socket is answered again after a read; and its
// datagrams to UDP 443 are refused, so QUIC falls back to TCP, but not its
// connect() to it, which glibc's address sort makes.
func TestUnderTheSwitchAReplyToTheAppsUDPIsACaptureLoss(t *testing.T) {
	if !inOwnPIDNamespace(t) {
		return
	}
	h, cg := provenUDPHooks(t)

	echo, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		buf := make([]byte, 16)
		for {
			_, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo([]byte("pong"), from)
		}
	}()
	port := echo.LocalAddr().(*net.UDPAddr).Port

	cgf, err := os.Open(cg)
	if err != nil {
		t.Fatal(err)
	}
	defer cgf.Close()
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	cmd.Env = append(os.Environ(), udpHelperEnv+"="+strconv.Itoa(port))
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgf.Fd())}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	out := bufio.NewReader(stdout)
	line, _ := out.ReadString('\n')
	if want := "ready connect443=<nil> send443=operation not permitted sendto443=operation not permitted"; strings.TrimSpace(line) != want {
		t.Fatalf("helper: %q, want %q: a connect() to UDP 443 through, the datagrams refused (EPERM)", line, want)
	}
	// replies is the udp_reply count, read once every report the kernel
	// made before the read is in (DrainCaptureLosses): one read, no polling.
	// Each read moves the kernel's epoch on.
	replies := func() models.CaptureLoss {
		t.Helper()
		if err := h.DrainCaptureLosses(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := h.CaptureLossesErr(); err != nil {
			t.Fatal(err)
		}
		for _, l := range h.CaptureLosses() {
			if l.Kind == models.CaptureLossUDPReply {
				return l
			}
		}
		return models.CaptureLoss{}
	}
	if l := replies(); l.Count != 1 || !strings.Contains(l.What, fmt.Sprintf("127.0.0.1:%d answered from port %d", port, port)) {
		t.Fatalf("udp_reply loss %+v, want one, naming 127.0.0.1:%d", l, port)
	}
	// The same socket answered again, after the read above: counted again.
	if _, err := stdin.Write([]byte("again\n")); err != nil {
		t.Fatal(err)
	}
	if line, _ := out.ReadString('\n'); strings.TrimSpace(line) != "ready again" {
		t.Fatalf("helper: %q, want its second exchange answered", line)
	}
	if l := replies(); l.Count != 2 {
		t.Fatalf("udp_reply loss %+v, want two: the socket's second reply came after a read", l)
	}
}

func cgroupV2Root(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(data), "\n") {
		if f := strings.Fields(l); len(f) > 2 && f[2] == "cgroup2" {
			return f[1]
		}
	}
	t.Fatal("no cgroup v2 mount")
	return ""
}

// A reply of a long-lived socket that lands between a read's move of the
// epoch and its count (the run's mark, here) is charged by the next read,
// not that one: its epoch stays open into test 1, where the kernel reports
// the socket's live reply no more, so test 1 still carries the loss. The
// watch goroutine is not started: the ring is read by hand, to its end, so
// the interleaving is exact.
func TestAReplyBetweenAReadsEpochMoveAndItsCountTaintsTheNextTest(t *testing.T) {
	if !inOwnPIDNamespace(t) {
		return
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	spec, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
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
	root := cgroupV2Root(t)
	cg, err := os.MkdirTemp(root, "keploy-udp-epoch-")
	if err != nil {
		t.Fatalf("create a cgroup: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cg) })
	for _, p := range []struct {
		prog   *ebpf.Program
		attach ebpf.AttachType
	}{
		{h.objects.K_connect4, ebpf.AttachCGroupInet4Connect},
		{h.objects.K_udp4Sendmsg, ebpf.AttachCGroupUDP4Sendmsg},
	} {
		l, err := link.AttachCgroup(link.CgroupOptions{Path: cg, Attach: p.attach, Program: p.prog})
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		t.Cleanup(func() { _ = l.Close() })
	}
	w, err := startUDPWatch(&h.objects, cg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.close)
	h.udp = w // no watch goroutine: drained by hand below

	var info structs.AgentInfo
	info.KeployAgentNsPid = uint32(os.Getpid())
	if info.KeployAgentInode, info.KeployAgentDev, err = GetSelfInodeNumber(); err != nil {
		t.Fatal(err)
	}
	info.IsDocker = 1
	info.DNSPort = -1
	info.Flags = structs.FlagProvenUDP
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
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)

	echo, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		buf := make([]byte, 16)
		for {
			_, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo([]byte("pong"), from)
		}
	}()
	port := echo.LocalAddr().(*net.UDPAddr).Port
	cgf, err := os.Open(cg)
	if err != nil {
		t.Fatal(err)
	}
	defer cgf.Close()
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	cmd.Env = append(os.Environ(), udpHelperEnv+"="+strconv.Itoa(port))
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgf.Fd())}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	out := bufio.NewReader(stdout)
	if line, _ := out.ReadString('\n'); !strings.HasPrefix(line, "ready connect443=") {
		t.Fatalf("helper: %q", line)
	}
	again := func() {
		t.Helper()
		if _, err := stdin.Write([]byte("again\n")); err != nil {
			t.Fatal(err)
		}
		if line, _ := out.ReadString('\n'); strings.TrimSpace(line) != "ready again" {
			t.Fatalf("helper: %q", line)
		}
	}
	// The ingress program runs before the datagram is queued, so once the
	// helper read its reply, any report is committed in the ring: a
	// non-blocking read to the ring's end handles every one.
	drain := func() {
		t.Helper()
		w.rd.SetDeadline(time.Unix(1, 0)) // a deadline past: read to the ring's end, never wait
		for {
			rec, err := w.rd.Read()
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var e udpReplyEvent
			if len(rec.RawSample) != udpReplyEventSize {
				t.Fatalf("report of %d bytes", len(rec.RawSample))
			}
			if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &e); err != nil {
				t.Fatal(err)
			}
			w.handle(e)
		}
	}
	udp := func(ls models.CaptureLosses) uint64 {
		for _, l := range ls {
			if l.Kind == models.CaptureLossUDPReply {
				return l.Count
			}
		}
		return 0
	}

	drain()
	if n := udp(h.CaptureLosses()); n != 1 {
		t.Fatalf("first reply: udp_reply %d, want 1", n)
	}
	// The run's mark (resetSession -> Agent.CaptureLosses -> snapshot), with
	// the socket answered between the epoch's move and the read, and the
	// report handled by then.
	w.closeEpoch()
	again()
	drain()
	mark := w.counts()
	// Test 1: the same socket answered live.
	again()
	drain()
	now := h.CaptureLosses()
	if err := h.CaptureLossesErr(); err != nil {
		t.Fatal(err)
	}
	lost := now.Since(mark)
	t.Logf("mark %+v; test 1's end %+v; charged to test 1: %+v", mark, now, lost)
	if udp(lost) == 0 {
		t.Fatalf("FALSE PASS: the socket was answered live during test 1, and test 1 has no udp_reply loss")
	}
}

// provenUDPHooks loads keploy's main object as the agent does, starts the UDP
// watch under the proven switch on a cgroup of the test's own (where the
// helpers run, and the programs keploy attaches at the root are attached
// instead), and registers this process as the agent: in a PID namespace of
// its own, every process there that calls socket() is a member.
func provenUDPHooks(t *testing.T) (*Hooks, string) {
	t.Helper()
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	spec, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
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

	// A cgroup of the test's own, where the helper runs and the programs
	// keploy attaches at the root are attached instead.
	root := cgroupV2Root(t)
	cg, err := os.MkdirTemp(root, "keploy-udp-test-")
	if err != nil {
		t.Fatalf("create a cgroup: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cg) })
	for _, p := range []struct {
		prog   *ebpf.Program
		attach ebpf.AttachType
	}{
		{h.objects.K_connect4, ebpf.AttachCGroupInet4Connect},
		{h.objects.K_udp4Sendmsg, ebpf.AttachCGroupUDP4Sendmsg},
	} {
		l, err := link.AttachCgroup(link.CgroupOptions{Path: cg, Attach: p.attach, Program: p.prog})
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		t.Cleanup(func() { _ = l.Close() })
	}
	// As the hooks' load does: the watch, then the flag it returns.
	t.Setenv(provenswitch.Env, "1")
	flags, err := h.startProvenUDP(cg)
	if err != nil || flags&structs.FlagProvenUDP == 0 {
		t.Fatalf("startProvenUDP under the switch = %#x, %v; want FlagProvenUDP", flags, err)
	}
	t.Cleanup(func() { h.udp.close() })

	var info structs.AgentInfo
	info.KeployAgentNsPid = uint32(os.Getpid())
	if info.KeployAgentInode, info.KeployAgentDev, err = GetSelfInodeNumber(); err != nil {
		t.Fatal(err)
	}
	info.IsDocker = 1
	info.DNSPort = -1
	info.Flags = flags
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
	return h, cg
}

// startUDPHelper runs the UDP helper (runUDPHelper) with arg, in cg, and
// returns its stdin and its stdout's reader.
func startUDPHelper(t *testing.T, cg, arg string) (io.Writer, *bufio.Reader) {
	t.Helper()
	cgf, err := os.Open(cg)
	if err != nil {
		t.Fatal(err)
	}
	defer cgf.Close()
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	cmd.Env = append(os.Environ(), udpHelperEnv+"="+arg)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgf.Fd())}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	return stdin, bufio.NewReader(stdout)
}

// udpLoss is the hooks' udp_reply count, read once every report the kernel
// made before the read is in.
func udpLoss(t *testing.T, h *Hooks) uint64 {
	t.Helper()
	if err := h.DrainCaptureLosses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.CaptureLossesErr(); err != nil {
		t.Fatal(err)
	}
	for _, l := range h.CaptureLosses() {
		if l.Kind == models.CaptureLossUDPReply {
			return l.Count
		}
	}
	return 0
}

// Under the switch an application's own UDP server (bound, asked by a client
// that is no member, answering each request) is answered live by nothing:
// its clients' requests are no answers. The watch is live all the same: a
// member's client answered by a real server is counted.
func TestUnderTheSwitchAnAppsUDPServerIsNotAnsweredLive(t *testing.T) {
	if !inOwnPIDNamespace(t) {
		return
	}
	h, cg := provenUDPHooks(t)
	in, out := startUDPHelper(t, cg, "server")
	line, _ := out.ReadString('\n')
	port, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "ready "))
	if err != nil {
		t.Fatalf("server helper: %q", line)
	}
	cl, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	buf := make([]byte, 16)
	for i := 0; i < 2; i++ {
		if _, err := in.Write([]byte("serve\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := cl.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		if line, _ := out.ReadString('\n'); strings.TrimSpace(line) != "ready served" {
			t.Fatalf("server helper: %q", line)
		}
		if n, err := cl.Read(buf); err != nil || string(buf[:n]) != "pong" {
			t.Fatalf("the client's answer %d: %q, %v", i, buf[:n], err)
		}
	}
	if n := udpLoss(t, h); n != 0 {
		t.Fatalf("%d udp_reply losses, want none: the server's clients' requests were counted as live answers", n)
	}

	echo, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		b := make([]byte, 16)
		for {
			_, from, err := echo.ReadFrom(b)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo([]byte("pong"), from)
		}
	}()
	_, cout := startUDPHelper(t, cg, strconv.Itoa(echo.LocalAddr().(*net.UDPAddr).Port))
	if line, _ := cout.ReadString('\n'); !strings.HasPrefix(line, "ready connect443=") {
		t.Fatalf("client helper: %q", line)
	}
	if n := udpLoss(t, h); n != 1 {
		t.Fatalf("%d udp_reply losses, want 1: a member's client answered by a real server", n)
	}
}
