//go:build linux

// Linux only, as the wire rows it builds on (wireAgent, scope_proven_test.go).

package agent

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// shimHooks are hooks that intercept through a library each process of the
// run loads (coreAgent.ShimReporter, macOS's shim): armed holds the processes
// that announced it, asked what the agent asked, and told, in order, each
// claimant the agent named and each read of the counts.
type shimHooks struct {
	countHooks
	armed map[int]bool
	asked []string
	told  []string
}

func (h *shimHooks) ShimClaimedBy(pid int) {
	h.told = append(h.told, "claimed by "+strconv.Itoa(pid))
}

// CaptureLosses has told say each read of the hooks' counts.
func (h *shimHooks) CaptureLosses() models.CaptureLosses {
	h.told = append(h.told, "read")
	return nil
}

func (h *shimHooks) ShimArmed(pid int, timeFile string) (bool, string) {
	h.asked = append(h.asked, strconv.Itoa(pid)+" "+timeFile)
	if h.armed[pid] {
		return true, ""
	}
	return false, "it never said HELLO"
}

// A test process that does not carry the shim is neither captured nor
// mocked: its begin is refused, naming the process and why, and opens
// nothing.
func TestABeginOfATestProcessWithoutTheShimIsRefused(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	h := &shimHooks{armed: map[int]bool{}}
	a.Hooks = h
	claimSelf(t, a)
	_, serr := a.ProvenBegin(context.Background(), begin("TestUnshimmed"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
	require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
	require.Contains(t, serr.Message, "o::TestUnshimmed")
	require.Contains(t, serr.Message, strconv.Itoa(os.Getpid()))
	require.Contains(t, serr.Message, "it never said HELLO")
	require.Equal(t, []string{strconv.Itoa(os.Getpid()) + " "}, h.asked)
	a.provenMu.Lock()
	open := a.proven.open
	a.provenMu.Unlock()
	require.Empty(t, open, "the refused begin opened no test")
}

// On a run whose clock keploy froze, a test process without the shim reads
// the real clock: its begin is refused clock_not_frozen, and the agent asked
// about the run's own clock record.
func TestABeginOfATestProcessOffTheFrozenClockIsRefused(t *testing.T) {
	useHooks(t, &clockHooks{file: "/run/kclk/t"})
	a, _ := wireAgent(t, models.MODE_RECORD)
	h := &shimHooks{armed: map[int]bool{}}
	a.Hooks = h
	claimSelf(t, a)
	_, serr := a.ProvenBegin(context.Background(), begin("TestUnfrozen"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrClockNotFrozen, serr.Code)
	require.Contains(t, serr.Message, "it never said HELLO")
	require.Equal(t, []string{strconv.Itoa(os.Getpid()) + " /run/kclk/t"}, h.asked)
}

// A test process that carries the shim begins its test.
func TestABeginOfAShimmedTestProcessOpensTheTest(t *testing.T) {
	useHooks(t, &clockHooks{file: "/run/kclk/t"})
	a, _ := wireAgent(t, models.MODE_RECORD)
	a.Hooks = &shimHooks{armed: map[int]bool{os.Getpid(): true}}
	claimSelf(t, a)
	_, serr := a.ProvenBegin(context.Background(), begin("TestShimmed"))
	require.Nil(t, serr)
	a.provenMu.Lock()
	open := a.proven.open
	a.provenMu.Unlock()
	require.Equal(t, "o::TestShimmed", open[0].id)
}

// The claim tells the shim's hooks which process claimed the run, before the
// read of the counts its first test is counted from: a program the claimant
// started before its claim, and runs still, is in that read, and none the
// hooks found before it is judged against another claimant. A claim again of
// the same process tells nothing new.
func TestAClaimTellsTheShimHooksItsProcessBeforeItReadsTheCounts(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	h := &shimHooks{armed: map[int]bool{os.Getpid(): true}}
	a.Hooks = h
	claimSelf(t, a)
	me := "claimed by " + strconv.Itoa(os.Getpid())
	require.NotEmpty(t, h.told)
	require.Equal(t, me, h.told[0], "the hooks are told the claimant before any read of the counts")
	require.Contains(t, h.told[1:], "read", "the claim reads the counts after telling")
	h.told = nil
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "o2"}, os.Getpid())
	require.Nil(t, serr)
	require.NotContains(t, h.told, me)
}

// shimInstanceHooks are shim hooks (shimHooks) that also put each test
// worker in a keploy instance of its own: asked holds each process the
// agent asked about, in order.
type shimInstanceHooks struct {
	instanceHooks
	armed map[int]bool
	asked []int
}

func (h *shimInstanceHooks) ShimClaimedBy(int) {}

func (h *shimInstanceHooks) ShimArmed(pid int, _ string) (bool, string) {
	h.asked = append(h.asked, pid)
	if h.armed[pid] {
		return true, ""
	}
	return false, "it never said HELLO"
}

// In a run of test workers, a worker's begin asks whether that worker's own
// process carries the shim: a worker without it is refused by its pid, and
// one with it begins, whichever claimed first.
func TestAWorkersBeginAsksWhetherItsOwnProcessCarriesTheShim(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	unshimmed := child(t).Process.Pid
	h := &shimInstanceHooks{armed: map[int]bool{os.Getpid(): true}}
	a.Hooks = h
	claimWorker(t, a, unshimmed, "a", 2)
	claimWorker(t, a, os.Getpid(), "b", 2)
	_, serr := a.ProvenBegin(as(os.Getpid()), begin("TestShimmed"))
	require.Nil(t, serr, "the worker that carries the shim begins")
	_, serr = a.ProvenBegin(as(unshimmed), begin("TestUnshimmed"))
	require.NotNil(t, serr, "the worker without the shim is refused")
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
	require.Contains(t, serr.Message, strconv.Itoa(unshimmed))
	require.Equal(t, []int{os.Getpid(), unshimmed}, h.asked)
}
