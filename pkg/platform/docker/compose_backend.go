//go:build !darwin

package docker

import (
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
)

// ComposeLibrarySupported reports whether this build can drive compose
// in-process. See compose_backend_darwin.go for why darwin cannot.
const ComposeLibrarySupported = true

// newComposeBackend returns the compose library's own implementation of the
// service interface.
//
// It lives in its own file for ONE reason: this is the only place that imports
// github.com/docker/compose/v2/pkg/compose, and that package is what makes the
// library unbuildable for darwin without cgo. Keeping the import here means
// composelib.go itself — and everything that only needs pkg/api — compiles on
// every platform.
func newComposeBackend(dockerCli command.Cli) (api.Compose, error) {
	return compose.NewComposeService(dockerCli), nil
}
