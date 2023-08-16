package main

import (
	"fmt"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	v "github.com/hashicorp/go-version"
	"go.keploy.io/server/cmd"
)

// version is the version of the server and will be injected during build by ldflags
// see https://goreleaser.com/customization/build/

var version string

const logo string = `
       ▓██▓▄
    ▓▓▓▓██▓█▓▄
     ████████▓▒
          ▀▓▓███▄      ▄▄   ▄               ▌
         ▄▌▌▓▓████▄    ██ ▓█▀  ▄▌▀▄  ▓▓▌▄   ▓█  ▄▌▓▓▌▄ ▌▌   ▓
       ▓█████████▌▓▓   ██▓█▄  ▓█▄▓▓ ▐█▌  ██ ▓█  █▌  ██  █▌ █▓
      ▓▓▓▓▀▀▀▀▓▓▓▓▓▓▌  ██  █▓  ▓▌▄▄ ▐█▓▄▓█▀ █▓█ ▀█▄▄█▀   █▓█
       ▓▌                           ▐█▌                   █▌
        ▓
`

func getKeployVersion() string {

	repo, err := git.PlainOpen(".")
	if err != nil {
		return "v0.1.0-dev"
	}

	tagIter, err := repo.Tags()
	if err != nil {
		return "v0.1.0-dev"
	}

	var latestTag string
	var latestTagVersion *v.Version

	err = tagIter.ForEach(func(tagRef *plumbing.Reference) error {
		tagName := tagRef.Name().Short()
		tagVersion, err := v.NewVersion(tagName)
		if err == nil {
			if latestTagVersion == nil || latestTagVersion.LessThan(tagVersion) {
				latestTagVersion = tagVersion
				latestTag = tagName
			}
		}
		return nil
	})

	if err != nil {
		return "v0.1.0-dev"
	}

	return latestTag + "-dev"
}

func main() {
	if version == "" {
		version = getKeployVersion()
	}

	fmt.Println(logo, " ")
	fmt.Printf("%v\n\n", version)
	cmd.Execute()
}
