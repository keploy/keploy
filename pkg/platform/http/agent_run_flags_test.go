package http

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// A `docker run` command's published ports, networks and host entries go to
// the agent container; a `docker compose` command keeps its flags, its -p
// above all, which names its project.
func TestRunFlagsOf(t *testing.T) {
	cmd, ports, networks, hosts, _ := runFlagsOf("docker run -p 8080:8080 --network shop --add-host db:10.0.0.2 --name app img:1", utils.DockerRun)
	if cmd != "docker run --name app img:1" {
		t.Errorf("docker run: command %q", cmd)
	}
	if !reflect.DeepEqual(ports, []string{"-p 8080:8080"}) || !reflect.DeepEqual(networks, []string{"shop"}) || !reflect.DeepEqual(hosts, []string{"db:10.0.0.2"}) {
		t.Errorf("docker run: ports %v, networks %v, hosts %v", ports, networks, hosts)
	}

	for _, c := range []string{
		"docker compose -p shop up",
		"docker compose -p shop -f deploy/compose.yaml up --build",
		"docker-compose -p shop up",
	} {
		var dns models.AppDNS
		cmd, ports, networks, hosts, dns = runFlagsOf(c, utils.DockerCompose)
		if cmd != c || ports != nil || networks != nil || hosts != nil || !dns.Empty() {
			t.Errorf("%q: became %q with ports %v, networks %v, hosts %v; a compose command keeps its project name", c, cmd, ports, networks, hosts)
		}
	}
}

// A `docker run` command's resolver settings, in each spelling docker takes,
// go to the agent container: under its network mode Docker refuses the app's
// own --dns ("conflicting options: dns and the network mode") and ignores its
// --dns-search and --dns-option, as the app shares the agent's resolv.conf.
// Until keploy took them out, keploy record -c "docker run --dns ..." failed
// at the app's start, exit 125.
func TestRunFlagsOfTakesTheAppsResolverSettings(t *testing.T) {
	cmd, _, _, _, dns := runFlagsOf(`docker run --dns 10.0.0.2 --dns=10.0.0.3 --dns-search corp.example --dns-search="svc.example" --dns-option ndots:2 --dns-option='timeout:1' --dns-opt=rotate -p 8080:8080 --name app img:1`, utils.DockerRun)
	if cmd != "docker run --name app img:1" {
		t.Errorf("command %q still carries a resolver flag", cmd)
	}
	want := models.AppDNS{
		Servers: []string{"10.0.0.2", "10.0.0.3"},
		Search:  []string{"corp.example", "svc.example"},
		Options: []string{"ndots:2", "timeout:1", "rotate"},
	}
	if !reflect.DeepEqual(dns, want) {
		t.Errorf("dns %+v, want %+v", dns, want)
	}
	// A flag that only starts like one is left alone.
	cmd, _, _, _, dns = runFlagsOf("docker run --dnsx 1 --name app img:1", utils.DockerRun)
	if cmd != "docker run --dnsx 1 --name app img:1" || !dns.Empty() {
		t.Errorf("--dnsx: command %q, dns %+v", cmd, dns)
	}
}
