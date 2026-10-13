//go:build linux

package agent

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// portContainers resolves each destination to a postgres container of its
// own, one per destination port.
type portContainers struct{}

func (portContainers) Lookup(dst netip.AddrPort) (containerkey.Container, bool, error) {
	return containerkey.Container{ID: fmt.Sprint(dst.Port()), Image: "postgres:16", Port: 5432}, true, nil
}
func (portContainers) Bind(context.Context)               {}
func (portContainers) Watch(context.Context, *zap.Logger) {}
func (portContainers) Endpoint() string                   { return "unix:///fake.sock" }

// useTestKeyer makes the agent's keyer one over portContainers for the test,
// and returns the key a connection to port gets now.
func useTestKeyer(t *testing.T) func(port int) string {
	t.Helper()
	saved := containerkey.Default
	k := containerkey.NewKeyer(portContainers{})
	containerkey.Default = k
	t.Cleanup(func() { containerkey.Default = saved })
	return func(port int) string {
		_, key, _ := k.Name(nil, "", fmt.Sprintf("127.0.0.1:%d", port))
		return key
	}
}

// Under the wire (scope protocol v1) the open test is the scope its own
// containers are numbered in, after the run's (design v14 §3.1), from its
// begin to its end. Numbers a test gives are not the run's, so the run's next
// container shows whether the test's scope closed.
func TestTheWiresOpenTestNumbersItsOwnContainers(t *testing.T) {
	key := useTestKeyer(t)
	a, _ := wireAgent(models.MODE_RECORD)
	ctx := context.Background()
	const k = "container:postgres:16:5432#"

	claimSelf(t, a)
	require.Equal(t, k+"1", key(40001), "the run's container")
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	require.Equal(t, k+"2", key(40002), "TestA's own container, after the run's")
	require.Equal(t, k+"1", key(40001), "the run's container keeps its number inside a test")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, k+"2", key(40003), "after TestA ended, the run numbers on from its own")

	// A refused begin opens nothing: the run numbers on.
	_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "TestNoSkip"})
	require.NotNil(t, serr)
	require.Equal(t, k+"3", key(40004))
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	require.Equal(t, k+"4", key(40005), "the refused begin numbered 40004 in a test of its own")
}

// A test whose process went away is closed by keploy (failOpen), and so is
// its numbering.
func TestATestKeployFailsClosesItsNumbering(t *testing.T) {
	key := useTestKeyer(t)
	a, _ := wireAgent(models.MODE_RECORD)
	ctx := context.Background()
	gone := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "g"}, gone.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestCrash"))
	require.Nil(t, serr)
	require.Equal(t, "container:postgres:16:5432#1", key(40001), "TestCrash's own container")
	kill(t, gone)
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "o"}, os.Getpid())
	require.Nil(t, serr)
	require.Equal(t, models.ScopeOutcomeFailed, windowOf(t, a, "g::TestCrash").Outcome)
	require.Equal(t, "container:postgres:16:5432#1", key(40002), "the numbering of the test keploy failed stayed open")
}

// A test the replay skips opens no numbering of its own: what is reached
// while it is skipped is the run's.
func TestASkippedTestNumbersNothing(t *testing.T) {
	key := useTestKeyer(t)
	a, _ := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestGated": "flaky"}))
	require.Equal(t, "container:postgres:16:5432#1", key(40001))
	b, serr := a.ProvenBegin(ctx, begin("TestGated"))
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionSkip, b.Action)
	require.Equal(t, "container:postgres:16:5432#2", key(40002))
	_, serr = a.ProvenBegin(ctx, begin("TestReal"))
	require.Nil(t, serr)
	require.Equal(t, "container:postgres:16:5432#3", key(40003), "the skipped test numbered 40002 in a test of its own")
}
