package utils

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// CheckRequiredPermissions verifies if the current process has the specific capabilities
// required for Keploy to function correctly in Docker mode.
// On non-Linux systems, this check remains a no-op as capabilities are Linux-specific.
func CheckRequiredPermissions() error {
	if runtime.GOOS != "linux" {
		return nil
	}

	// Read /proc/self/status to find "CapEff".
	content, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return fmt.Errorf("failed to read process status: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	var capEffHex string
	for _, line := range lines {
		if strings.HasPrefix(line, "CapEff:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				capEffHex = parts[1]
			}
			break
		}
	}

	if capEffHex == "" {
		return fmt.Errorf("could not find CapEff in /proc/self/status")
	}

	// Parse hex string to uint64
	var capUtils uint64
	_, err = fmt.Sscanf(capEffHex, "%x", &capUtils)
	if err != nil {
		return fmt.Errorf("failed to parse CapEff hex '%s': %w", capEffHex, err)
	}

	// Define required bits based on Linux Capability constants
	// CAP_NET_ADMIN = 12
	// CAP_SYS_PTRACE = 19
	// CAP_SYS_RESOURCE = 24
	// CAP_BPF = 38
	// CAP_PERFMON = 39

	required := map[string]uint{
		"NET_ADMIN":    12,
		"SYS_PTRACE":   19,
		"SYS_RESOURCE": 24,
		"BPF":          38,
		"PERFMON":      39,
	}

	var missing []string
	for name, bit := range required {
		// specific check is: (cap_mask & (1 << bit))
		if (capUtils & (uint64(1) << bit)) == 0 {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required capabilities: %s. Please ensure you have the permissions with: NET_ADMIN, SYS_PTRACE, SYS_RESOURCE, BPF, PERFMON", strings.Join(missing, ", "))
	}

	return nil
}
