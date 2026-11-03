package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/agent/starts"
)

// A connection the kernel stamped with a live test worker's instance is
// that worker's; once the worker has ended, one stamped with its value is
// from no live worker: it is given no instance, so it is never the use of
// the test of a later claim of the same id, and it names the ended worker.
// One stamped with no instance names none.
func TestARealConnectionOfAnEndedWorkerIsNoWorkers(t *testing.T) {
	v := agent.InstanceValue(7, 3)
	starts.Default.InstanceBegan(v)
	t.Cleanup(func() { starts.Default.InstanceEnded(v) })
	dest := &agent.NetworkAddress{Instance: v, KernelPid: 42}
	require.Equal(t, realdeps.Origin{Instance: v, Pid: 42}, realOrigin(dest))
	starts.Default.InstanceEnded(v)
	require.Equal(t, realdeps.Origin{Pid: 42, Ended: "7 (claim 4 of that id)"}, realOrigin(dest))
	require.Equal(t, realdeps.Origin{Pid: 42}, realOrigin(&agent.NetworkAddress{KernelPid: 42}))
	require.Equal(t, realdeps.Origin{}, realOrigin(nil))
}

// liveInstances makes vs live test workers' instances while t runs
// (starts.Default): a connection stamped with one is its worker's.
func liveInstances(t *testing.T, vs ...uint32) {
	for _, v := range vs {
		starts.Default.InstanceBegan(v)
		t.Cleanup(func() { starts.Default.InstanceEnded(v) })
	}
}
