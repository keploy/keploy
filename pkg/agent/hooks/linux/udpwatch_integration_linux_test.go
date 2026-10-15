//go:build linux && pidmap_integration

// Root-only, like pidmaps_integration_linux_test.go: it loads keploy's BPF
// object and attaches cgroup programs.

package linux

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
)

// udpHelperEnv makes a re-executed test binary a UDP client: it connects a
// UDP socket to 127.0.0.1:PORT, sends, waits for the reply, tries UDP 443
// (a connect(), a datagram on it, an unconnected send), says what each did;
// then, at a line on stdin, sends and waits for the reply again on the
// first socket, and lives until its stdin closes.
const udpHelperEnv = "KEPLOY_UDP_TEST_HELPER"

func runUDPHelper(port string) int {
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
	if _, err := in.ReadString('\n'); err != nil {
		return 0
	}
	if err := exchange(); err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	fmt.Println("ready again")
	_, _ = in.ReadString(0)
	return 0
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
	w, err := startUDPWatch(&h.objects, cg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.close)
	go w.watch(zap.NewNop())
	h.udp = w

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
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0) // registers this process as the agent
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
