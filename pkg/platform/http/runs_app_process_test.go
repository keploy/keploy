package http

import "testing"

// keploy's agent client runs the application's command as a process of its
// own and returns when it exits, and says so: replay waits for that
// application with no time limit, as its exit ends the wait.
func TestTheAgentClientSaysItRunsTheAppsProcess(t *testing.T) {
	if !(&AgentClient{}).RunsAppProcess() {
		t.Fatal("the agent client says its Run does not run the application's process")
	}
}
