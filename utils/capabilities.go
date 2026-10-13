package utils

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Linux capability numbers (include/uapi/linux/capability.h). Spelled out
// here, not taken from golang.org/x/sys/unix, because this file builds on
// every platform.
const (
	CapNetAdmin    = 12
	CapSysPtrace   = 19
	CapSysAdmin    = 21
	CapSysResource = 24
	CapPerfmon     = 38
	CapBPF         = 39
)

// EffectiveCapabilities returns the process's effective capability set, the
// CapEff mask in /proc/self/status, as a bit mask indexed by capability
// number. Linux only.
func EffectiveCapabilities() (uint64, error) {
	content, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("failed to read process status: %w", err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		hex, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		hex = strings.TrimSpace(hex)
		caps, err := strconv.ParseUint(hex, 16, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse CapEff hex '%s': %w", hex, err)
		}
		return caps, nil
	}
	return 0, fmt.Errorf("could not find CapEff in /proc/self/status")
}

// ReadPerfEventParanoid returns the value of the kernel.perf_event_paranoid
// knob at path (/proc/sys/kernel/perf_event_paranoid, or a test's file). The
// knob is not namespaced: read from inside a container, it is the host's.
func ReadPerfEventParanoid(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	level, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("%s holds %q, not an integer: %w", path, strings.TrimSpace(string(b)), err)
	}
	return level, nil
}

// CheckRequiredPermissions verifies if the current process has the specific capabilities
// required for Keploy to function correctly in Docker mode.
// On non-Linux systems, this check remains a no-op as capabilities are Linux-specific.
func CheckRequiredPermissions() error {
	if runtime.GOOS != "linux" {
		return nil
	}

	caps, err := EffectiveCapabilities()
	if err != nil {
		return err
	}

	required := map[string]uint{
		"NET_ADMIN":    CapNetAdmin,
		"SYS_PTRACE":   CapSysPtrace,
		"SYS_RESOURCE": CapSysResource,
		"BPF":          CapBPF,
		"PERFMON":      CapPerfmon,
	}

	var missing []string
	for name, bit := range required {
		if caps&(uint64(1)<<bit) == 0 {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required capabilities: %s. Please ensure you have the permissions with: NET_ADMIN, SYS_PTRACE, SYS_RESOURCE, BPF, PERFMON", strings.Join(missing, ", "))
	}

	return nil
}
