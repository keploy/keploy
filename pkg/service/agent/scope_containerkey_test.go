//go:build linux

package agent

import (
	"context"
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// portContainers resolves each destination to a postgres container of its
// own, one per destination port, every one configured the same: two of them
// used in one scope cannot be told apart, so the keyer withholds their key
// (containerkey.Withheld), which shows which scope a use fell in.
type portContainers struct{}

func (portContainers) Lookup(dst netip.AddrPort) (containerkey.Container, bool, error) {
	return containerkey.Container{ID: fmt.Sprint(dst.Port()), Image: "postgres:16", Port: 5432, Fingerprint: "0123456789ab"}, true, nil
}
func (portContainers) Fingerprint(context.Context, string) (string, bool) { return "", false }
func (portContainers) Pending(netip.AddrPort) ([]string, bool)            { return nil, true }
func (portContainers) AwaitReadsOf(context.Context, []string)             {}
func (portContainers) Bind(context.Context)                               {}
func (portContainers) Watch(context.Context, *zap.Logger)                 {}
func (portContainers) Endpoint() string                                   { return "unix:///fake.sock" }

// useTestKeyer makes the agent's keyer one over portContainers for the test,
// and returns what a call to port's container is now: Keyed when no other
// container was used in the scope open now, Withheld when one was.
func useTestKeyer(t *testing.T) func(port int) containerkey.State {
	t.Helper()
	saved := containerkey.Default
	k := containerkey.NewKeyer(portContainers{})
	containerkey.Default = k
	t.Cleanup(func() { containerkey.Default = saved })
	return func(port int) containerkey.State {
		id := fmt.Sprint(port)
		k.Name(nil, id, fmt.Sprintf("127.0.0.1:%d", port))
		_, st := containerkey.StateFromContext(containerkey.WithConn(context.Background(), k, id))
		return st
	}
}

const (
	keyed    = containerkey.Keyed
	withheld = containerkey.Withheld
)

// Under the wire (scope protocol v1) the open test is a scope of its own,
// from its begin to its end: its containers are told apart from each other,
// not from the run's.
func TestTheWiresOpenTestIsAScopeOfItsOwn(t *testing.T) {
	use := useTestKeyer(t)
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()

	claimSelf(t, a)
	require.Equal(t, keyed, use(40001), "the run's container")
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	require.Equal(t, keyed, use(40002), "TestA's own container, alone in TestA")
	require.Equal(t, withheld, use(40001), "the run's container, configured as TestA's, used in TestA")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, keyed, use(40001), "after TestA ended, the run's scope again, where only it was used")

	// A refused begin opens nothing: the run's scope goes on.
	_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "TestNoSkip"})
	require.NotNil(t, serr)
	require.Equal(t, withheld, use(40004), "the refused begin opened a scope of its own")
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	require.Equal(t, keyed, use(40005), "TestB's own container, alone in TestB")
}

// A test keploy closes itself, failing it because it was still open at the
// run's final read (failOpen), closes its scope too.
func TestATestKeployFailsClosesItsScope(t *testing.T) {
	use := useTestKeyer(t)
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	proc := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, proc.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestLingering"))
	require.Nil(t, serr)
	require.Equal(t, keyed, use(40001), "TestLingering's own container")
	require.Equal(t, models.ScopeOutcomeFailed, windowOf(t, a, "k::TestLingering").Outcome)
	require.Equal(t, keyed, use(40002), "the scope of the test keploy failed stayed open")
}

// Outside tests each test process (instance) is a scope of its own: what an
// earlier test process used does not make a later one's containers
// ambiguous.
func TestEachTestProcessIsAScopeOfItsOwn(t *testing.T) {
	use := useTestKeyer(t)
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	require.Equal(t, keyed, use(40001), "a container of the run's, before any test process")

	first := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "a"}, first.Process.Pid)
	require.Nil(t, serr)
	require.Equal(t, keyed, use(40002), "the first test process's container")
	kill(t, first)

	claimSelf(t, a)
	require.Equal(t, keyed, use(40003), "the next test process's container, told apart from the first one's")
	require.Equal(t, withheld, use(40002), "two containers configured the same in one test process")
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	require.Equal(t, keyed, use(40004), "a test's own container, alone in the test")
}

// A test the replay skips opens no scope of its own: what is reached while it
// is skipped is the run's.
func TestASkippedTestOpensNoScope(t *testing.T) {
	use := useTestKeyer(t)
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestGated": "flaky"}, nil, ""))
	require.Equal(t, keyed, use(40001))
	b, serr := a.ProvenBegin(ctx, begin("TestGated"))
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionSkip, b.Action)
	require.Equal(t, withheld, use(40002), "the skipped test opened a scope of its own")
	_, serr = a.ProvenBegin(ctx, begin("TestReal"))
	require.Nil(t, serr)
	require.Equal(t, keyed, use(40003), "the next test's own container, alone in it")
}
