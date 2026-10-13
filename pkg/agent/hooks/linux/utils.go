//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/utils"
)

func GetSelfInodeNumber() (uint64, uint64, error) {
	p := filepath.Join("/proc", "self", "ns", "pid")

	f, err := os.Stat(p)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to stat %s for keploy pid namespace: %w", p, err)
	}
	st := f.Sys().(*syscall.Stat_t)
	return st.Ino, uint64(st.Dev), nil
}

// privilegeFailure tags an eBPF load the kernel refused for want of
// privileges, so the agent's exit status can say so (utils.ExitCodeFor).
// Every step of the load is a privileged kernel operation — raising the
// memlock rlimit, bpf(2), perf_event_open(2), attaching to a cgroup — so a
// permission error from any of them (EPERM, EACCES) means the same thing: run
// elevated, or grant the capabilities.
//
// Except from the verifier, which rejects a program with the same EACCES
// (an invalid context access, a misaligned read). A load is only verified
// once the kernel has allowed it, and the verifier always leaves its reasons
// in the log keploy asks for, while a load refused for want of privileges is
// refused before that log is written; cilium/ebpf tells its MEMLOCK hint
// apart the same way. A rejected program is keploy's to fix, not something
// any privilege can.
func privilegeFailure(err error) error {
	var ve *ebpf.VerifierError
	if errors.As(err, &ve) && len(ve.Log) > 0 {
		return err
	}
	if errors.Is(err, os.ErrPermission) && !errors.Is(err, utils.ErrPrivilegeRequired) {
		return fmt.Errorf("%w: %w", utils.ErrPrivilegeRequired, err)
	}
	return err
}

// What diagnosePerfEventRefusal reads. Variables so a test can point them at
// files and capabilities of its own.
var (
	// perfEventParanoidPath is the sysctl that gates perf_event_open(2). Not
	// namespaced: inside a container it is the host's.
	perfEventParanoidPath = "/proc/sys/kernel/perf_event_paranoid"
	// procVersionPath names the running kernel and who built it: Debian's
	// names its builder, "(debian-kernel@lists.debian.org)". A kernel only
	// compiled with Debian's gcc (Proxmox's, a self-built one) says "Debian"
	// too, and is not Debian's.
	procVersionPath = "/proc/version"
	// effectiveCapabilities is the agent's CapEff mask.
	effectiveCapabilities = utils.EffectiveCapabilities
)

// perfEventOpenStep is how cilium/ebpf's link.Tracepoint (v0.21,
// link/perf_event.go) wraps the error of its perf_event_open(2) call. The same
// attach can be refused at its other steps, reading the event's id from
// tracefs ("reading trace event ID of ...") and attaching the program ("cannot
// create bpf perf link", or on kernels before 5.15 "setting perf event bpf
// program"), and perf_event_paranoid explains none of them. An error that does
// not carry it is left alone: at worst a refusal keeps its bare message, as
// before.
const perfEventOpenStep = "opening tracepoint perf event"

// perfEventEnv is what explainPerfEventRefusal knows of the agent and the host
// when perf_event_open(2) was refused.
type perfEventEnv struct {
	// eacces: the kernel answered EACCES. Debian's restriction answers only
	// that; a seccomp profile or an LSM typically answers EPERM.
	eacces        bool
	paranoid      int
	paranoidKnown bool
	capsKnown     bool
	sysAdmin      bool
	// kernel is the running kernel's release, or empty.
	kernel string
	// debian: the running kernel is Debian's, which carries the restriction.
	debian bool
}

// diagnosePerfEventRefusal names why the kernel refused the agent the
// perf_event_open(2) of a tracepoint attach, and returns any other error as it
// came. The cause stays wrapped, so privilegeFailure still tags it and the
// agent still exits ExitPrivilegeRequired.
//
// "permission denied" alone sends whoever reads it after the agent's
// capabilities, and the usual cause is not there. Debian's kernel
// (CONFIG_SECURITY_PERF_EVENTS_RESTRICT) refuses perf_event_open with EACCES to
// every process without CAP_SYS_ADMIN, CAP_PERFMON included, while
// kernel.perf_event_paranoid is above 2, and it boots at 3. An agent with
// exactly the capabilities it is meant to have (the k8s-proxy chart's sidecar:
// CAP_BPF and CAP_PERFMON, no CAP_SYS_ADMIN) loads every program and is then
// refused here, on a Debian host nothing has lowered the sysctl on since boot
// (keploy/enterprise pipeline 10867). Mainline kernels let CAP_PERFMON through
// at every level, and so does Ubuntu's, which boots at 4 (checked on
// 6.8.0-142-generic): there a refusal of an agent that loaded its programs is a
// security policy's.
//
// By the time this call is made, the agent has loaded its tracepoint program,
// which the kernel allows only with CAP_PERFMON or CAP_SYS_ADMIN held in the
// host's user namespace: one of the two is held, and a user namespace of its
// own is not the cause.
func diagnosePerfEventRefusal(err error) error {
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), perfEventOpenStep) {
		return err
	}
	env := perfEventEnv{eacces: errors.Is(err, unix.EACCES)}
	if level, rerr := utils.ReadPerfEventParanoid(perfEventParanoidPath); rerr == nil {
		env.paranoid, env.paranoidKnown = level, true
	}
	if caps, cerr := effectiveCapabilities(); cerr == nil {
		env.capsKnown = true
		env.sysAdmin = caps&(1<<utils.CapSysAdmin) != 0
	}
	if b, rerr := os.ReadFile(procVersionPath); rerr == nil {
		// "Linux version <release> (...)".
		if f := strings.Fields(string(b)); len(f) > 2 {
			env.kernel = f[2]
		}
		env.debian = strings.Contains(string(b), "debian-kernel@lists.debian.org")
	}
	return explainPerfEventRefusal(err, env)
}

// explainPerfEventRefusal wraps err, a refused perf_event_open(2), in the
// cause env points at and what to change.
func explainPerfEventRefusal(err error, env perfEventEnv) error {
	const (
		restriction = "a Debian kernel refuses perf_event_open to any process without CAP_SYS_ADMIN, CAP_PERFMON included, while kernel.perf_event_paranoid is above 2"
		fix         = "to fix it, set kernel.perf_event_paranoid=2 on the host (sysctl -w kernel.perf_event_paranoid=2, and a file under /etc/sysctl.d/ to keep it after a reboot; under Kubernetes, on every node keploy records on), or grant keploy CAP_SYS_ADMIN"
		policy      = "a security policy refused perf_event_open: check the container's seccomp profile and the host's LSM (SELinux, AppArmor or a BPF LSM)"
	)
	host := "kernel.perf_event_paranoid could not be read"
	if env.paranoidKnown {
		host = fmt.Sprintf("kernel.perf_event_paranoid is %d", env.paranoid)
	}
	switch {
	case env.kernel != "" && env.debian:
		host += " (kernel " + env.kernel + ", Debian's)"
	case env.kernel != "":
		host += " (kernel " + env.kernel + ")"
	}
	caps := "keploy holds CAP_PERFMON but not CAP_SYS_ADMIN"
	if !env.capsKnown {
		caps = "keploy could not read its capabilities"
	}
	var why string
	switch {
	case env.capsKnown && env.sysAdmin:
		why = fmt.Sprintf("keploy holds CAP_SYS_ADMIN, which no kernel.perf_event_paranoid level refuses (%s), so %s", host, policy)
	case !env.eacces:
		// EPERM: not Debian's restriction, which answers EACCES.
		why = fmt.Sprintf("the kernel answered EPERM, not the EACCES of kernel.perf_event_paranoid's restriction (%s; %s), so %s", host, caps, policy)
	case env.paranoidKnown && env.paranoid > 2 && env.debian:
		why = fmt.Sprintf("%s and %s: %s; %s", host, caps, restriction, fix)
	case env.paranoidKnown && env.paranoid > 2:
		// Not Debian's kernel, or not known to be: a hardened host at 3 on a
		// mainline kernel admits CAP_PERFMON, and its SELinux refuses with
		// EACCES too.
		why = fmt.Sprintf("%s and %s: if this kernel restricts perf events as Debian's does (%s), %s; if not, it lets CAP_PERFMON through, so %s", host, caps, restriction, fix, policy)
	case env.paranoidKnown:
		why = fmt.Sprintf("%s, which lets CAP_PERFMON through, and %s, so %s", host, caps, policy)
	default:
		why = fmt.Sprintf("%s and %s: if it is above 2 on a kernel that restricts perf events as Debian's does (%s), %s; if not, %s", host, caps, restriction, fix, policy)
	}
	return fmt.Errorf("%s: %w", why, err)
}

// tracepointFailure tags a tracepoint that could not be attached because
// tracefs is not mounted — a container started without /sys/kernel/debug or
// /sys/kernel/tracing — as the environment's failure rather than keploy's.
func tracepointFailure(err error, tracefsMounted bool) error {
	if tracefsMounted {
		return err
	}
	return fmt.Errorf("%w: %w", utils.ErrEnvironmentUnsupported, err)
}

// tracefsMounted reports whether tracefs can be reached where the tracepoint
// API is looked up: the paths, and filesystem types, cilium/ebpf's own lookup
// accepts (internal/tracefs getTracefsPath). That lookup reports a missing
// mount only as a plain string, so this is how that cause is told apart from
// every other reason an attach can fail.
func tracefsMounted() bool {
	for _, m := range []struct {
		path  string
		magic int64
	}{
		{"/sys/kernel/tracing", unix.TRACEFS_MAGIC},
		{"/sys/kernel/debug/tracing", unix.TRACEFS_MAGIC},
		{"/sys/kernel/debug/tracing", unix.DEBUGFS_MAGIC},
	} {
		var st unix.Statfs_t
		if unix.Statfs(m.path, &st) == nil && int64(st.Type) == m.magic {
			return true
		}
	}
	return false
}
