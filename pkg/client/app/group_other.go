//go:build !linux

package app

import "go.keploy.io/server/v3/utils"

// GroupRunsOn says whether a process the application's command started
// runs on in the command's own process group once the command has exited
// (utils.GroupRunsOn): the application still running, which a run's caller
// waits on before keploy stops it (models.RunOptions.BeforeStop). False for
// an app not started yet. A docker command's group holds no process once
// the command has exited: its app runs in a container.
func (a *App) GroupRunsOn() bool {
	cmd := a.native.Load()
	if cmd == nil || cmd.Process == nil {
		return false
	}
	return utils.GroupRunsOn(cmd.Process.Pid)
}
