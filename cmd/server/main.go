package main

import (
	"github.com/go-git/go-git/v5"
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

	repo, err := git.PlainOpen(".")
	if err != nil {
		return "0.1.0-dev"
	}

	tagIter, err := repo.Tags()
	if err != nil {
		return "0.1.0-dev"
	}

	tagRef, err := tagIter.Next()
	if err != nil {
		return "0.1.0-dev"
	}

	tag := tagRef.Name().Short()

	return tag + "-dev"
}
