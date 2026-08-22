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
// LD_PRELOAD: loading Keploy's interception shim into the application means
// creating the process suspended, mapping the DLL into it and only then
// resuming it. That has to happen at the exact moment the process is created,
// which is here — but the code that knows how to do it lives in
// pkg/agent/hooks/winshim, which imports this package. A registered function
// inverts the dependency.
//
// It is nil unless Windows interception is active, so a docker run and every
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
