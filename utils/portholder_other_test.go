//go:build !linux

package utils

import (
	"strings"
	"testing"
)

// Off Linux keploy reads no port's holder: PortTaken says it cannot, and
// gives no Linux reason (a namespace, an unreadable /proc) it cannot know.
func TestPortTakenOffLinuxSaysTheHolderCannotBeRead(t *testing.T) {
	got := PortTaken(1)
	if !strings.Contains(got, "keploy cannot read what holds a port on this platform") || strings.Contains(got, "namespace") {
		t.Fatalf("PortTaken(1) off Linux = %q", got)
	}
}
