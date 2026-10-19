package replay

import (
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// agentInstanceWatcher is an Instrumentation that can tell which agent
// process answers it (models.AgentInstanceWatch): the keploy agent client.
type agentInstanceWatcher interface {
	WatchAgentInstance() models.AgentInstanceWatch
}

// watchAgent begins, under the proven switch, the watch a test set is judged
// with: what the set is judged on (the calls no mock answered, what its
// tests consumed, what keploy could not capture) is held in the agent
// process's memory, so the set is judged only if one process answered for
// all of it. An agent started again in its place mid-set (kubelet
// restarting a crashed sidecar, docker restarting a container) answers at
// the same address holding none of what the one before saw, and its reads
// say nothing was missed. nil when the run has no agent, or the
// instrumentation cannot tell which process answers it.
func (r *Replayer) watchAgent() models.AgentInstanceWatch {
	if !r.instrument || r.noAgent || !provenswitch.On() {
		return nil
	}
	w, ok := r.instrumentation.(agentInstanceWatcher)
	if !ok {
		return nil
	}
	return w.WatchAgentInstance()
}

// agentReplacedReason is the reason a set whose agent was replaced while it
// ran is not judged.
func agentReplacedReason(rep models.AgentReplacement) string {
	return fmt.Sprintf("not judged: keploy's agent was started again while the test-set ran (agent process %s answered, then %s), and what the agent before saw (the calls no mock answered, what the tests consumed, what keploy could not capture) went with it; find why the agent restarted (its container's restart count, an OOM kill, its log), then run the test-set again",
		rep.First, rep.Next)
}
