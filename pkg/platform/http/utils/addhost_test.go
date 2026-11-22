package utils

import (
	"slices"
	"testing"
)

// The app's --add-host entries, in either spelling, quoted or not, are taken
// out of its docker run command: under keploy the app joins the agent
// container's network, where Docker refuses them ("conflicting options:
// custom host-to-IP mapping and the network mode"), and the agent carries
// them.
func TestExtractAddHosts(t *testing.T) {
	cmd, hosts := ExtractAddHosts(`docker run --rm --name app --add-host db.test:10.1.2.3 --add-host=api.test:host-gateway --add-host "v6.test:[::1]" -p 8080:8080 app:latest`)
	if want := "docker run --rm --name app -p 8080:8080 app:latest"; cmd != want {
		t.Errorf("command %q, want %q", cmd, want)
	}
	if want := []string{"db.test:10.1.2.3", "api.test:host-gateway", "v6.test:[::1]"}; !slices.Equal(hosts, want) {
		t.Errorf("hosts %q, want %q", hosts, want)
	}
	if cmd, hosts := ExtractAddHosts("docker run app:latest"); cmd != "docker run app:latest" || hosts != nil {
		t.Errorf("a command with no host entry became %q, %q", cmd, hosts)
	}
}
