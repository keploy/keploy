//go:build darwin

package docker

import (
	"errors"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v2/pkg/api"
)

// ComposeLibrarySupported is false on darwin, and callers must fall back to
// shelling out to `docker compose`.
//
// github.com/docker/compose/v2/pkg/compose imports pkg/watch, which on darwin
// imports github.com/fsnotify/fsevents — a cgo-only package (watcher_darwin.go
// is //go:build darwin and its symbols come from the C wrapper). Keploy's
// darwin binaries are cross-compiled from Linux with CGO_ENABLED=0, so linking
// it fails outright:
//
//	fsevents.go:21:8: undefined: EventFlags
//
// Enabling cgo is not an option here: cross-compiling darwin from Linux with
// cgo needs an osxcross toolchain that the build images do not carry.
//
// Nothing is lost. The library exists so the in-POD compose runner can work in
// a distroless image with no `docker` binary, and that runner is linux-only. A
// darwin build is somebody's laptop, where `docker compose` is what installed
// the engine in the first place — so the shell-out path this falls back to is
// both available and exactly what keploy did before the library existed.
const ComposeLibrarySupported = false

var errComposeLibraryUnsupported = errors.New(
	"the compose library is not linked into darwin builds (fsevents needs cgo); use the docker CLI path")

func newComposeBackend(command.Cli) (api.Compose, error) {
	return nil, errComposeLibraryUnsupported
}
