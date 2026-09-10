//go:build windows

package utils

import (
	"os/exec"
	"sync/atomic"
)

// nativeAppStarter, when set, replaces cmd.Start() for the launch of the user's
// application.
//
// It exists because Windows has no equivalent of DYLD_INSERT_LIBRARIES or
// LD_PRELOAD: loading an interception shim into the application means creating
// the process suspended, mapping the DLL into it and only then resuming it.
// That has to happen at the exact moment the process is created, which is here.
//
// This build has no Windows interception backend — native macOS and Windows
// interception ship in the Community and Enterprise editions — so nothing here
// ever registers a starter. The seam exists so those builds can, exactly as
// cli/provider.RegisterNativeCommandSupport lets them widen the platform gate.
// Same shape, same reason: the extension point lives here, the implementation
// does not.
//
// It is nil unless such a build installed one, so a docker run and every
// non-Windows platform take exactly the path they took before.
var nativeAppStarter atomic.Pointer[func(cmd *exec.Cmd) error]

// SetNativeAppStarter installs the starter used for the application launch.
// Passing nil clears it.
func SetNativeAppStarter(fn func(cmd *exec.Cmd) error) {
	if fn == nil {
		nativeAppStarter.Store(nil)
		return
	}
	nativeAppStarter.Store(&fn)
}

// startApplication starts cmd through the registered starter when there is one,
// and with a plain cmd.Start() otherwise.
//
// kind gates this deliberately: ExecuteCommand also runs the user's pre- and
// post-test scripts, which pass utils.Empty and must never be instrumented —
// they are keploy's own orchestration, not the application under test. Only a
// native application launch passes utils.Native.
func startApplication(cmd *exec.Cmd, kind CmdType) error {
	if kind != Native {
		return cmd.Start()
	}
	if fn := nativeAppStarter.Load(); fn != nil {
		return (*fn)(cmd)
	}
	return cmd.Start()
}
