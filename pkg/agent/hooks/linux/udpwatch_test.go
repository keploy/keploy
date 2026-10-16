//go:build linux

package linux

import (
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// With the proven switch off the UDP watch starts nothing and turns nothing
// on in the kernel: no programs (the hooks' objects are not even loaded
// here), no flag, no losses.
func TestTheUDPWatchStaysOffWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	h := &Hooks{logger: zap.NewNop()}
	flags, err := h.startProvenUDP("/nonexistent-cgroup")
	if err != nil || flags != 0 {
		t.Fatalf("startProvenUDP with the switch off = %#x, %v; want 0, nil", flags, err)
	}
	if flags&structs.FlagProvenUDP != 0 || h.udp != nil {
		t.Fatalf("with the switch off the UDP watch started: flags %#x, watch %v", flags, h.udp)
	}
	if got := h.udpLosses(); len(got) != 0 {
		t.Fatalf("with the switch off the UDP watch counted %+v", got)
	}
	if err := h.CaptureLossesErr(); err != nil {
		t.Fatalf("with the switch off, CaptureLossesErr = %v", err)
	}
}

// Under the switch the watch needs the hooks' objects: with none loaded it
// fails, and turns nothing on.
func TestTheUDPWatchFailsWithoutItsObjectsUnderTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &Hooks{logger: zap.NewNop()}
	flags, err := h.startProvenUDP("/nonexistent-cgroup")
	if err == nil || flags != 0 || h.udp != nil {
		t.Fatalf("startProvenUDP with no objects under the switch = %#x, %v (watch %v); want 0 and an error", flags, err, h.udp)
	}
}

// A watch that cannot tell what it counted (an epoch it could not move on, a
// report it could not read) leaves the capture losses unknown, never 0.
func TestAUDPWatchFailureIsUnknownNeverZero(t *testing.T) {
	h := &Hooks{logger: zap.NewNop(), udp: &udpWatch{}}
	if err := h.CaptureLossesErr(); err != nil {
		t.Fatalf("a watch that has not failed: CaptureLossesErr = %v", err)
	}
	h.udp.fail(errors.New("a UDP reply report of 40 bytes"))
	if err := h.CaptureLossesErr(); err == nil || !strings.Contains(err.Error(), "40 bytes") {
		t.Fatalf("CaptureLossesErr = %v, want the watch's failure", err)
	}
}
