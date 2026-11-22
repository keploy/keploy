//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// keployCgroupV2Mountpoint is the container-local path at which keploy mounts a
// cgroup2 (unified) hierarchy when the host exposes none (legacy cgroup v1). It
// is deliberately not under the bind-mounted /sys/fs/cgroup so the mount stays
// private to the agent's mount namespace and never mutates host state.
const keployCgroupV2Mountpoint = "/run/keploy/cgroupv2"

// mountCgroupV2 mounts the kernel's unified cgroup v2 hierarchy at a
// keploy-owned mount point and returns it. It is called only when no cgroup2
// mount is already present (legacy cgroup v1 host).
//
// The mount is performed with mount(2) directly — no `mount` binary is spawned,
// so the distroless agent image stays shell-/tool-free. It requires
// CAP_SYS_ADMIN and an unconfined seccomp/AppArmor profile; both are granted to
// the agent service only when the host is detected to lack cgroup v2 (see
// GenerateKeployAgentService), keeping the agent least-privileged elsewhere.
//
// The mount is intentionally NOT unmounted on teardown:
//   - In the containerized agent (cloud replay / docker) it lives in the
//     container's mount namespace and is reclaimed automatically when the
//     container exits, so there is no host mutation to undo.
//   - In native mode it persists, but it is a benign, controller-less *view* of
//     the kernel's always-present v2 hierarchy (as systemd itself keeps mounted
//     at /sys/fs/cgroup/unified); /run is tmpfs so it clears on reboot. It is
//     idempotent — a re-run finds it via findCgroupV2Mount and skips
//     re-mounting, so mounts never accumulate — and leaving it avoids yanking
//     the cgroup path out from under a concurrent keploy process that reused it.
func mountCgroupV2(logger *zap.Logger) (string, error) {
	logger.Info("no cgroup2 (unified) hierarchy is mounted; mounting one for eBPF hooks (host appears to use legacy cgroup v1)",
		zap.String("mountpoint", keployCgroupV2Mountpoint))

	if err := os.MkdirAll(keployCgroupV2Mountpoint, 0o755); err != nil {
		return "", fmt.Errorf("cgroup2 not mounted and keploy could not create a mount point at %s: %w", keployCgroupV2Mountpoint, err)
	}

	if err := unix.Mount("none", keployCgroupV2Mountpoint, "cgroup2", 0, ""); err != nil {
		switch {
		case errors.Is(err, unix.EPERM):
			return "", fmt.Errorf("cgroup2 not mounted and keploy could not mount it (%w): the host uses legacy cgroup v1 and the keploy-agent lacks the privileges to mount a cgroup2 hierarchy. Run the agent with CAP_SYS_ADMIN and an unconfined AppArmor profile, or switch the host to unified cgroup v2 (boot with systemd.unified_cgroup_hierarchy=1)", err)
		case errors.Is(err, unix.ENODEV):
			return "", fmt.Errorf("cgroup2 not mounted and this kernel provides no cgroup2 (unified) hierarchy (%w): a kernel with cgroup v2 support (>= 4.5) is required", err)
		default:
			return "", fmt.Errorf("cgroup2 not mounted and keploy failed to mount a cgroup2 hierarchy at %s: %w", keployCgroupV2Mountpoint, err)
		}
	}

	// The mounted view is rooted at the mounting process's cgroup namespace
	// root, so the eBPF cgroup hooks attach there. On a legacy cgroup v1 host —
	// the only host that reaches this fallback — the v2 hierarchy carries no
	// controller delegation, so every process (agent + app) sits at that root
	// and a root attach captures them regardless of cgroup namespace. This does
	// assume the agent runs under the host cgroup namespace (Docker's default on
	// cgroup v1 hosts); if a non-default cgroupns=private is forced on a host
	// that *does* delegate the v2 tree, the app may fall outside the attached
	// subtree — the symptom is captured-nothing, so surface the assumption here.
	logger.Info("mounted cgroup2 hierarchy for eBPF hooks (attaching at the host cgroup-namespace root)",
		zap.String("mountpoint", keployCgroupV2Mountpoint))
	return keployCgroupV2Mountpoint, nil
}
