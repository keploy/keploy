package mockdb

import "testing"

// TestUseGobFormatPerInstance verifies the per-session mock-format seam
// (O5): a MockYaml's own MockFormat decides gob-vs-structured, so two
// concurrent sessions can use different formats instead of the process
// global. Empty MockFormat falls back to the global default (parity).
func TestUseGobFormatPerInstance(t *testing.T) {
	// Neutralize the env override so the per-instance field is the
	// deciding factor.
	t.Setenv("KEPLOY_MOCK_FORMAT", "")

	if g := (&MockYaml{MockFormat: mockFormatGob}).useGobFormat(); !g {
		t.Fatal("MockFormat=gob should select gob")
	}
	if g := (&MockYaml{MockFormat: "yaml"}).useGobFormat(); g {
		t.Fatal("MockFormat=yaml must not select gob")
	}
	// Empty → global default (configuredMockFormat is "" at rest → not gob).
	if g := (&MockYaml{}).useGobFormat(); g {
		t.Fatal("empty MockFormat with no global set must not select gob")
	}
}
