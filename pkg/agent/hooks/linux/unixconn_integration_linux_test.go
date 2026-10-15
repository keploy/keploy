//go:build linux && pidmap_integration

// Root-only, like pidmaps_integration_linux_test.go: it loads keploy's BPF
// objects (the main one and unixconn) into the running kernel.

package linux

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// unixHelperEnv makes a re-executed test binary an AF_UNIX helper: "listen
// PATH" binds and listens on PATH, "connect PATH" connects to it; either says
// "ready" (or "helper: <error>") and lives until its stdin closes.
const unixHelperEnv = "KEPLOY_UNIX_TEST_HELPER"

func runUnixHelper(step string) int {
	f := strings.Fields(step)
	if len(f) != 2 {
		fmt.Println("helper: bad step", step)
		return 2
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		switch f[0] {
		case "listen":
			if err = unix.Bind(fd, &unix.SockaddrUnix{Name: f[1]}); err == nil {
				err = unix.Listen(fd, 8)
			}
		case "connect":
			err = unix.Connect(fd, &unix.SockaddrUnix{Name: f[1]})
		default:
			err = fmt.Errorf("unknown step %s", f[0])
		}
	}
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString(0)
	return 0
}

func startUnixHelper(t *testing.T, step string) {
	t.Helper()
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	cmd.Env = append(os.Environ(), unixHelperEnv+"="+step)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("unix helper (%s): got %q (%v), want ready", step, line, err)
	}
}

// The agent side of the AF_UNIX report, against keploy's own objects: an
// application process that connects to a Unix socket no member owns is a
// unix_socket capture loss in Hooks.CaptureLosses (what Agent.CaptureLosses,
// and so every test after it under the proven switch, is charged with); one
// to a member's socket is not. Run in a PID namespace of its own, so that no
// host process becomes a member while it runs.
func TestAnAppConnectToAUnixSocketNoMemberOwnsIsACaptureLoss(t *testing.T) {
	if !inOwnPIDNamespace(t) {
		return
	}
	// The syscall tracepoints, as on a kernel without BPF trampolines,
	// when asked (the test binary is re-run in the namespace with it).
	if os.Getenv("KEPLOY_UNIX_TEST_FALLBACK") == "1" {
		unixFallbackOnly = true
		t.Cleanup(func() { unixFallbackOnly = false })
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outside, own := filepath.Join(dir, "outside.sock"), filepath.Join(dir, "member.sock")
	// Listening before the agent is registered: never a member.
	startUnixHelper(t, "listen "+outside)

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
	var info structs.AgentInfo
	info.KeployAgentNsPid = uint32(os.Getpid())
	if info.KeployAgentInode, info.KeployAgentDev, err = GetSelfInodeNumber(); err != nil {
		t.Fatal(err)
	}
	info.IsDocker = 1
	info.DNSPort = -1
	if err := h.SendAgentInfo(info); err != nil {
		t.Fatal(err)
	}
	// This process is the agent, recognised by its own socket().
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// The report runs under the proven switch only.
	t.Setenv(provenswitch.Env, "1")
	h.startUnixConn(ctx)
	h.unixMu.Lock()
	off := h.unix.off
	h.unixMu.Unlock()
	if off != "" {
		t.Fatalf("the AF_UNIX report did not load: %s", off)
	}

	// A member's own socket first, then the outsider's: the reports come in
	// that order, so when the outsider's has been counted, the member's would
	// have been too.
	startUnixHelper(t, "listen "+own)
	startUnixHelper(t, "connect "+own)
	startUnixHelper(t, "connect "+outside)
	for {
		got := h.CaptureLosses()
		if len(got) == 1 && got[0].What == outside {
			if got[0].Kind != models.CaptureLossUnixSocket || got[0].Count != 1 {
				t.Fatalf("capture losses %+v, want one unix_socket loss, the outsider's socket: the member's own was counted", got)
			}
			return
		}
		if len(got) > 1 || (len(got) == 1 && got[0].Count > 1) {
			t.Fatalf("capture losses %+v, want only the outsider's socket", got)
		}
		// No deadline: the test binary's -timeout bounds it.
		time.Sleep(time.Millisecond)
	}
}
