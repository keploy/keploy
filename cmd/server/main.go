package main

import (
	"os/exec"
	"strings"

	"go.keploy.io/server/server"
)

// version is the version of the server and will be injected during build by ldflags
// see https://goreleaser.com/customization/build/

var version string

func main() {
	// main method to start Keploy server
	if version == "" {
		version = getKeployVersion()
	}
	server.Server(version)
}

func getKeployVersion() string {
	cmd := exec.Command("git", "describe", "--tags")
	out, err := cmd.Output()
	if err != nil {
		return "0.1.0-dev"
	}
	version := strings.TrimSpace(string(out))
	version = version[0:6] + "-dev"
	return version
}
