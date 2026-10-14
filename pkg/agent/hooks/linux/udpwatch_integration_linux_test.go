//go:build linux && pidmap_integration

// Root-only, like pidmaps_integration_linux_test.go: it loads keploy's BPF
// object and attaches cgroup programs.

package linux

import (
	"bufio"
	"fmt"
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
)

// udpHelperEnv makes a re-executed test binary a UDP client: it connects a
// UDP socket to 127.0.0.1:PORT, sends, waits for the reply, sends to UDP 443
// too, says what both did, and lives until its stdin closes.
const udpHelperEnv = "KEPLOY_UDP_TEST_HELPER"

func runUDPHelper(port string) int {
	p, _ := strconv.Atoi(port)
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err == nil {
		err = unix.Connect(fd, &unix.SockaddrInet4{Port: p, Addr: [4]byte{127, 0, 0, 1}})
	}
	if err == nil {
		_, err = unix.Write(fd, []byte("ping"))
	}
	buf := make([]byte, 16)
	if err == nil {
		_, err = unix.Read(fd, buf)
	}
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	quic, _ := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	qerr := unix.Sendto(quic, []byte("q"), 0, &unix.SockaddrInet4{Port: 443, Addr: [4]byte{127, 0, 0, 1}})
	fmt.Printf("ready quic=%v\n", qerr)
	_, _ = bufio.NewReader(os.Stdin).ReadString(0)
	return 0
}

// Under the proven switch, keploy's own object: the application's UDP that a
// real server answers is a udp_reply capture loss in Hooks.CaptureLosses,
// and its send to UDP 443 is refused, so QUIC falls back to TCP.
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
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if !strings.HasPrefix(line, "ready quic=operation not permitted") {
		t.Fatalf("helper: %q, want its send to UDP 443 refused (EPERM)", line)
	}
	for {
		for _, l := range h.CaptureLosses() {
			if l.Kind == models.CaptureLossUDPReply {
				if l.Count != 1 || !strings.Contains(l.What, fmt.Sprintf("127.0.0.1:%d answered from port %d", port, port)) {
					t.Fatalf("udp_reply loss %+v, want one, naming 127.0.0.1:%d", l, port)
				}
				return
			}
		}
		time.Sleep(time.Millisecond) // no deadline but the test binary's -timeout
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
