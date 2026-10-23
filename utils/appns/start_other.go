//go:build !linux

package appns

import (
	"os/exec"

	"go.uber.org/zap"
)

// Start starts cmd as cmd.Start does. Only Linux has mount namespaces, and
// neither macOS nor Windows reads an nsswitch.conf.
func Start(_ *zap.Logger, cmd *exec.Cmd) (done func(), err error) {
	return func() {}, cmd.Start()
}
