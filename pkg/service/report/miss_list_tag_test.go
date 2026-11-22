package report

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// keploy report names a missed call of the run's report whole, beside the
// path tag the report carries for it, the one the agent's miss line named
// it by; a call the report carries no tag for (an agent that tagged none)
// is named as before. keploy report runs in a process of its own, whose
// tags no line of the run names: it prints the stored tag, never its own.
func TestKeployReportNamesAMissBesideItsTag(t *testing.T) {
	var sb strings.Builder
	renderMissList(&sb, []models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "POST /oauth/token", PathTag: "#1f2e3d4c5b6a"},
		{Protocol: "HTTP", ActualSummary: "GET /profile"},
	})
	out := sb.String()
	if !strings.Contains(out, "  [HTTP] POST /oauth/token [path tag #1f2e3d4c5b6a]\n") {
		t.Errorf("keploy report names the tagged call as\n%s", out)
	}
	if !strings.Contains(out, "  [HTTP] GET /profile\n") || strings.Count(out, "path tag") != 1 {
		t.Errorf("keploy report names the untagged call as\n%s\nwant it alone, no tag of its own", out)
	}
}

// keploy report names a missed call's destination whole beside the host tag
// the report carries for it, the one the agent's line named it by, and a DNS
// question beside its tag: as the console does.
func TestKeployReportNamesAMissDestinationBesideItsHostTag(t *testing.T) {
	var sb strings.Builder
	renderMissList(&sb, []models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "GET /v1/items", Destination: "api.example.com:443", HostTag: "#3fa2c1d0e9b8"},
		{Protocol: "DNS", ActualSummary: "A tenant.example.com.", HostTag: "#0a1b2c3d4e5f"},
	})
	out := sb.String()
	for _, want := range []string{"  [HTTP] GET /v1/items  →  api.example.com:443 [host tag #3fa2c1d0e9b8]\n", "  [DNS] A tenant.example.com. [host tag #0a1b2c3d4e5f]\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("keploy report said\n%s\nwant it to hold %q", out, want)
		}
	}
}
