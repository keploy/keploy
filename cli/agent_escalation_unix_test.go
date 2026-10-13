//go:build unix

package cli

import (
	"syscall"
	"testing"

	"go.keploy.io/server/v3/utils"
)

// The agent's CLI forces a stop with utils.EscalationSignal when it may not
// signal the agent itself (sudo's): the agent command's PreRunE makes it kill
// the agent where it stands (utils.DieOnEscalation).
func TestAgentDiesOfTheEscalationSignal(t *testing.T) {
	_, agent, out, exited := startHangupAgent(t, false)
	if err := agent.Process.Signal(utils.EscalationSignal); err != nil {
		t.Fatal(err)
	}
	if st := awaitAgentEnd(t, agent, exited, out); !st.Signaled() || st.Signal() != syscall.SIGKILL {
		t.Fatalf("the agent ended with %v, want it killed (SIGKILL)\n%s", agent.ProcessState, out.String())
	}
}
