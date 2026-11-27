package docker

import (
	"slices"
	"testing"
)

func TestGenerateKeployVolumesIncludesHostBPFFSOutsideSys(t *testing.T) {
	vols := (&Impl{}).generateKeployVolumes()
	if !slices.Contains(vols, "/sys/fs/bpf:/run/keploy/host-bpffs") {
		t.Fatalf("generateKeployVolumes() = %v, missing /sys/fs/bpf:/run/keploy/host-bpffs", vols)
	}
	if !slices.Contains(vols, "/sys/fs/bpf:/sys/fs/bpf") {
		t.Fatalf("generateKeployVolumes() = %v, missing /sys/fs/bpf:/sys/fs/bpf", vols)
	}
}
