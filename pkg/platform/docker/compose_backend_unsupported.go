//go:build !composelib || darwin

package docker

import (
	"errors"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
)

// ComposeLibrarySupported is false here, and callers fall back to shelling out
// to `docker compose` — which is exactly what keploy did before the library
// existed, so nothing is lost by it.
//
// TWO reasons this file gets built.
//
//  1. No `composelib` tag. The library is reachable only through
//     Config.InMemoryCompose, which is tagged `json:"-" yaml:"-"
//     mapstructure:"-"` — it cannot be set from a config file, a flag or an env
//     var, and nothing in THIS repo sets it. Only enterprise does, for its cloud
//     replay and its in-pod compose runner. So an OSS build would link code it
//     can never execute -- measured on keploy's own binary (linux/amd64,
//     stripped), the tag adds 9.0 MiB with compose v5.5.1 (60.0 -> 69.0 MiB;
//     it added 42 MiB with compose v2) -- and that binary also ships inside the
//     agent image injected into every recorded application pod. Enterprise
//     builds with -tags composelib and gets the library; everyone else keeps
//     today's size.
//
//  2. darwin, tag or not. The library exists for enterprise's in-POD compose
//     runner, which is linux-only, and its cloud replay, which runs on linux.
//     A darwin build is somebody's laptop, where `docker compose` is what
//     installed the engine in the first place, so the shell-out this falls
//     back to is both available and what keploy has always done there.
//
// Under compose v2 darwin was also a constraint: its backend reached
// github.com/fsnotify/fsevents through pkg/watch, fsevents is cgo-only, and
// keploy's darwin binaries are cross-compiled from Linux with CGO_ENABLED=0,
// so linking it failed outright (fsevents.go:21:8: undefined: EventFlags).
// compose v5 keeps fsevents behind its own `fsnotify` build tag and links on
// darwin without cgo, so keeping darwin on the CLI is now a choice, not a
// constraint.
const ComposeLibrarySupported = false

var errComposeLibraryUnsupported = errors.New(
	"the compose library is not linked into this build: rebuild with -tags composelib " +
		"(darwin builds always use the docker CLI); use the docker CLI path")

func newComposeBackend(command.Cli, bool) (api.Compose, error) {
	return nil, errComposeLibraryUnsupported
}
