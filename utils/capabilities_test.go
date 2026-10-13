//go:build linux

package utils

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// The capability numbers this file spells out are the kernel's. CAP_PERFMON
// and CAP_BPF were swapped here once, so a process missing only one of them
// was told it lacked the other.
func TestCapabilityNumbersAreTheKernels(t *testing.T) {
	for name, pair := range map[string][2]int{
		"CAP_NET_ADMIN":    {CapNetAdmin, unix.CAP_NET_ADMIN},
		"CAP_SYS_PTRACE":   {CapSysPtrace, unix.CAP_SYS_PTRACE},
		"CAP_SYS_ADMIN":    {CapSysAdmin, unix.CAP_SYS_ADMIN},
		"CAP_SYS_RESOURCE": {CapSysResource, unix.CAP_SYS_RESOURCE},
		"CAP_PERFMON":      {CapPerfmon, unix.CAP_PERFMON},
		"CAP_BPF":          {CapBPF, unix.CAP_BPF},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %d here, %d in the kernel's headers", name, pair[0], pair[1])
		}
	}
}

// EffectiveCapabilities agrees with capget(2) for this process, whichever
// capabilities the test runs with.
func TestEffectiveCapabilitiesAgreesWithCapget(t *testing.T) {
	got, err := EffectiveCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		t.Skipf("capget: %v", err)
	}
	if want := uint64(data[0].Effective) | uint64(data[1].Effective)<<32; got != want {
		t.Fatalf("EffectiveCapabilities() = %#x, capget says %#x", got, want)
	}
}

func TestReadPerfEventParanoid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perf_event_paranoid")
	for content, want := range map[string]int{"3\n": 3, "-1\n": -1, " 2 \n": 2} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadPerfEventParanoid(path); err != nil || got != want {
			t.Errorf("ReadPerfEventParanoid of %q = %d, %v; want %d", content, got, err, want)
		}
	}
	for _, content := range []string{"x\n", "", "3 1\n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadPerfEventParanoid(path); err == nil {
			t.Errorf("ReadPerfEventParanoid of %q = %d, want an error", content, got)
		}
	}
	if _, err := ReadPerfEventParanoid(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing knob read without an error")
	}
}
