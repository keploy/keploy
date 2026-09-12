package docker

import (
	"testing"

	"go.keploy.io/server/v3/utils"
)

// Docker accepts "--name foo" and "--name=foo" interchangeably. ParseDockerCmd
// used to match only the spaced form, so the equals form produced an empty
// container name and an error - and the caller assigned the empty name anyway,
// leaving teardown and log capture pointed at "".
func TestParseDockerCmdFlagSpellings(t *testing.T) {
	cases := []struct {
		name          string
		cmd           string
		wantContainer string
		wantNetwork   string
		wantErr       bool
	}{
		{
			name:          "space form",
			cmd:           "docker run --name my-app --network keploy-network img",
			wantContainer: "my-app",
			wantNetwork:   "keploy-network",
		},
		{
			name:          "equals form",
			cmd:           "docker run --name=my-app --network=keploy-network img",
			wantContainer: "my-app",
			wantNetwork:   "keploy-network",
		},
		{
			name:          "mixed spellings",
			cmd:           "docker run --name=my-app --network keploy-network img",
			wantContainer: "my-app",
			wantNetwork:   "keploy-network",
		},
		{
			name:          "net alias in equals form",
			cmd:           "docker run --name my-app --net=keploy-network img",
			wantContainer: "my-app",
			wantNetwork:   "keploy-network",
		},
		{
			name:          "ports and other flags in between",
			cmd:           "docker run -d -p 8080:8080 --name=my-app -e A=B --network=keploy-network img",
			wantContainer: "my-app",
			wantNetwork:   "keploy-network",
		},
		{
			name:          "network missing is an error, container still reported",
			cmd:           "docker run --name=my-app img",
			wantContainer: "my-app",
			wantErr:       true,
		},
		{
			name:    "no name at all",
			cmd:     "docker run --network=keploy-network img",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			container, network, err := ParseDockerCmd(tc.cmd, utils.DockerRun, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseDockerCmd(%q) err = %v, wantErr %v", tc.cmd, err, tc.wantErr)
			}
			if container != tc.wantContainer {
				t.Errorf("container = %q, want %q", container, tc.wantContainer)
			}
			if network != tc.wantNetwork {
				t.Errorf("network = %q, want %q", network, tc.wantNetwork)
			}
		})
	}
}

// A flag value that merely contains the text of another flag must not be read
// as that flag. The previous regex scanned the raw string and did exactly that.
func TestParseDockerCmdDoesNotReadFlagsOutOfValues(t *testing.T) {
	container, network, err := ParseDockerCmd(
		"docker run --name=real --network=realnet -e MSG=--name -e OTHER=--network img",
		utils.DockerRun, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if container != "real" {
		t.Errorf("container = %q, want %q", container, "real")
	}
	if network != "realnet" {
		t.Errorf("network = %q, want %q", network, "realnet")
	}
}
