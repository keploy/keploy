package http

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/utils"
)

// A `docker run` command's published ports, networks and host entries go to
// the agent container; a `docker compose` command keeps its flags, its -p
// above all, which names its project.
func TestRunFlagsOf(t *testing.T) {
	cmd, ports, networks, hosts := runFlagsOf("docker run -p 8080:8080 --network shop --add-host db:10.0.0.2 --name app img:1", utils.DockerRun)
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
		cmd, ports, networks, hosts = runFlagsOf(c, utils.DockerCompose)
		if cmd != c || ports != nil || networks != nil || hosts != nil {
			t.Errorf("%q: became %q with ports %v, networks %v, hosts %v; a compose command keeps its project name", c, cmd, ports, networks, hosts)
		}
	}
}
