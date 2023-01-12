package main

import (
	"go.keploy.io/server/server"
)

// version is the version of the server and will be injected during build by ldflags
// see https://goreleaser.com/customization/build/

var version = server.DefaultVersion

func main() {
	// main method to start Keploy server
	server.Server(version)
}
