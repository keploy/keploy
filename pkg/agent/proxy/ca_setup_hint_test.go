package proxy

import (
	"context"
	"strings"
	"testing"

	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
)

func TestAHandshakeLostWhileWindowsHoldsNoKeployCANamesTheStep(t *testing.T) {
	c := &clientTLS{ctx: context.Background(), dst: "10.0.0.1:443"}
	if got, _ := c.lossText(); got != "to 10.0.0.1:443" {
		t.Fatalf("lossText with keploy's CA trusted = %q, want the destination only", got)
	}
	const why = "keploy's CA is not in this user's Windows trust store: trust it once"
	defer pTls.SetUntrustedStoreWhyForTest(why)()
	want := "to 10.0.0.1:443: " + why
	if got, _ := c.lossText(); got != want {
		t.Fatalf("lossText with keploy's CA untrusted = %q, want %q", got, want)
	}
}

func TestOnWindowsAFailedCASetupNamesNoVolumeAndNoElevation(t *testing.T) {
	win := caSetupNextStep("windows")
	for _, not := range []string{"docker", "/tmp/keploy-tls", "ca-certificates", "elevated"} {
		if strings.Contains(win, not) {
			t.Errorf("the Windows next step %q names %q", win, not)
		}
	}
	if !strings.Contains(win, "no administrator") {
		t.Errorf("the Windows next step %q does not say no administrator is needed", win)
	}
	if linux := caSetupNextStep("linux"); !strings.Contains(linux, "/tmp/keploy-tls") {
		t.Errorf("the Linux next step %q lost the shared volume", linux)
	}
}
