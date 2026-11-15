//go:build linux

// Linux only, as the wire rows it builds on (wireAgent, scope_proven_test.go).

package agent

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// runMemberHooks hold the run's processes themselves
// (coreAgent.RunMemberReporter, Windows's job object): held is each process
// they hold, by pid, with its creation time; asked is each pid the agent
// asked about, in order.
type runMemberHooks struct {
	countHooks
	held  map[int]uint64
	asked []int
}

func (h *runMemberHooks) HoldsInRun(pid int, start uint64) bool {
	h.asked = append(h.asked, pid)
	s, ok := h.held[pid]
	return ok && s == start
}

// A claim from a process that does not descend from keploy's client (on
// Windows, one whose parent exited cuts its chain) is taken when the hooks
// hold that process in the run, by its creation time. One they hold under
// another creation time (a later process on its number) is refused, and so is
// one they do not hold. A process that descends from the client is the run's
// without asking.
func TestAClaimTheHooksHoldInTheRunIsTakenWhereverItsChainWasCut(t *testing.T) {
	stranger := os.Getppid() // descends from no process of the run
	start, ok := procStart(stranger)
	require.True(t, ok)
	claim := func(h *runMemberHooks, pid int) *models.ScopeError {
		a, _ := wireAgent(t, models.MODE_TEST)
		a.config.Agent.ClientNSPID = uint32(os.Getpid()) // the run is this process's own children
		a.Hooks = h
		_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a"}, pid)
		return serr
	}

	h := &runMemberHooks{held: map[int]uint64{stranger: start + 1}}
	serr := claim(h, stranger)
	require.NotNil(t, serr, "held under another creation time")
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
	require.Equal(t, []int{stranger}, h.asked)

	h = &runMemberHooks{held: map[int]uint64{}}
	serr = claim(h, stranger)
	require.NotNil(t, serr, "not held")
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)

	h = &runMemberHooks{held: map[int]uint64{stranger: start}}
	require.Nil(t, claim(h, stranger), "a process the hooks hold in the run is the run's")
	require.Equal(t, []int{stranger}, h.asked)

	h = &runMemberHooks{held: map[int]uint64{}}
	require.Nil(t, claim(h, child(t).Process.Pid), "a child of the run's root is the run's")
	require.Empty(t, h.asked, "a process that descends from the client is not asked about")
}
