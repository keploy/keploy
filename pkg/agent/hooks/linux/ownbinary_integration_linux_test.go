//go:build linux && pidmap_integration

// Root-only, as pidmaps_integration_linux_test.go: it loads keploy's BPF
// object and creates a PID namespace.

package linux

import (
	"errors"
	"os"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// A keploy command run in the agent's container (the docker compose
// healthcheck's `keploy health`, a verb an operator runs there with `docker
// exec`) runs the agent's own file. The hooks leave it alone, as they leave
// the agent: it takes no place in target_namespace_pids or excluded_pids, so
// its calls (its telemetry among them) reach their real destinations instead
// of the agent's proxy. An application runs a file of its own and is a
// target, as before. The rule holds in every mode, and only keploy itself
// runs that exact file (the same inode on the same filesystem device): a
// keploy build under test runs another file, and every process that runs
// any other file, a keploy command's children among them, still counts as
// the application.
func TestKeploysOwnCommandInTheAgentsContainerTakesNoPlace(t *testing.T) {
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
	// As hooks.load does before loading.
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
	exit, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: "sched_process_exit", Program: h.objects.K_processExit})
	if err != nil {
		t.Fatalf("attach sched_process_exit: %v", err)
	}
	t.Cleanup(func() { _ = exit.Close() })
	sock, err := link.Tracepoint("syscalls", "sys_enter_socket", h.objects.SyscallProbeEntrySocket, nil)
	if err != nil {
		t.Fatalf("attach sys_enter_socket (is tracefs mounted?): %v", err)
	}
	t.Cleanup(func() { _ = sock.Close() })

	// This process is the agent, in docker mode: every other process of its
	// PID namespace is a target.
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
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)
	var agentTGID uint32
	if err := h.objects.KeployAgentKernelPidMap.Lookup(uint32(0), &agentTGID); err != nil || agentTGID == 0 {
		t.Fatalf("the agent did not register itself: %d, %v", agentTGID, err)
	}

	places := func() int {
		t.Helper()
		n := 0
		for _, m := range []*ebpf.Map{h.objects.TargetNamespacePids, h.objects.ExcludedPids} {
			var key, next uint32
			k := any(nil)
			for {
				err := m.NextKey(k, &next)
				if errors.Is(err, ebpf.ErrKeyNotExist) {
					break
				}
				if err != nil {
					t.Fatalf("walk %v: %v", m, err)
				}
				n++
				key = next
				k = &key
			}
		}
		return n
	}

	// keploy's own command: the agent's very file.
	startSocketHelperOf(t, "/proc/self/exe")
	if n := places(); n != 0 {
		t.Fatalf("a process that runs the agent's own file took %d places in the PID maps, want none", n)
	}
	// An application: a file of its own.
	startSocketHelperOf(t, appExe())
	if n := places(); n != 1 {
		t.Fatalf("an application's process took %d places in the PID maps, want one", n)
	}
}
