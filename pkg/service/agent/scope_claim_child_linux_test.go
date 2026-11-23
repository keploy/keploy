package agent

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A launcher that runs the test binary as its child claims the run for it:
// the claim is the child's, so the launcher's own claim is then refused as a
// second test process's, and once the child exits the run is free again.
func TestAClaimForARunningChildIsTheChilds(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	c := child(t)
	pid := c.Process.Pid
	start, ok := procStart(pid)
	require.True(t, ok, "the child runs")

	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "k", Child: pid}, os.Getpid())
	require.Nil(t, serr)
	a.provenMu.Lock()
	got := a.proven.claim
	a.provenMu.Unlock()
	require.Equal(t, claimant{pid: pid, start: start}, got, "the claim is the child's, with its own start time")

	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, os.Getpid())
	require.NotNil(t, serr, "the launcher is not the claimer: the child is, and it runs")
	require.Equal(t, models.ScopeErrOneTestProcess, serr.Code)
	require.Contains(t, serr.Message, fmt.Sprintf("while process %d still runs", pid))

	kill(t, c)
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, os.Getpid())
	require.Nil(t, serr, "the run is free once the child the claim was made for exited")
}

// startGrandchild starts a shell, in a group of its own, that starts a
// process and waits for it, and returns that grandchild's pid. The group is
// killed in t.Cleanup.
func startGrandchild(t *testing.T) int {
	t.Helper()
	sh := exec.Command("sh", "-c", "sleep 3600 & echo $!; wait")
	sh.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := sh.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, sh.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-sh.Process.Pid, syscall.SIGKILL)
		_ = sh.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	require.NoError(t, err)
	return pid
}

// A claim for a process that is not a running child of the caller is
// refused, and claims nothing: its grandchild, its own parent, itself, and a
// child that has exited.
func TestAClaimForAProcessThatIsNotTheCallersRunningChildIsRefused(t *testing.T) {
	ended := child(t)
	endedPid := ended.Process.Pid
	kill(t, ended)
	for _, tc := range []struct {
		name string
		pid  int
	}{
		{"its grandchild", startGrandchild(t)},
		{"its parent", os.Getppid()},
		{"itself", os.Getpid()},
		{"a child that exited", endedPid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := wireAgent(t, models.MODE_RECORD)
			_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "k", Child: tc.pid}, os.Getpid())
			require.NotNil(t, serr)
			require.Equal(t, models.ScopeErrNotInRun, serr.Code)
			require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
			require.Equal(t, fmt.Sprintf("process %d claimed this keploy run for process %d, which is not a running process it started", os.Getpid(), tc.pid), serr.Message)
			a.provenMu.Lock()
			got := a.proven.claim
			a.provenMu.Unlock()
			require.Equal(t, claimant{}, got, "the refused claim claims nothing")
		})
	}
}

// A claim for a running child is taken only when the child is itself one of
// the run's processes, as its caller must be, never because its parent is.
// A launcher the hooks hold in the run (on Windows, through the run's job
// object, its chain to keploy's client cut) that claims for a child the
// hooks do not hold, or hold under another creation time, is refused, and
// nothing is claimed. Once the hooks hold the child, by its own creation
// time, the claim is the child's. A child that exits before the hooks answer
// for it is refused as one that does not run.
func TestAClaimForAChildOutsideTheRunIsRefusedThoughItsLauncherIsInIt(t *testing.T) {
	launcher := os.Getpid()
	launcherStart, ok := procStart(launcher)
	require.True(t, ok)
	c := child(t).Process.Pid
	childStart, ok := procStart(c)
	require.True(t, ok)
	// keploy's client: neither the launcher nor its child descends from it.
	root := child(t).Process.Pid
	claim := func(held map[int]uint64) (*runMemberHooks, claimant, *models.ScopeError) {
		a, _ := wireAgent(t, models.MODE_RECORD)
		a.config.Agent.ClientNSPID = uint32(root)
		h := &runMemberHooks{held: held}
		a.Hooks = h
		_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "k", Child: c}, launcher)
		a.provenMu.Lock()
		got := a.proven.claim
		a.provenMu.Unlock()
		return h, got, serr
	}
	refused := fmt.Sprintf("process %d claimed this keploy run for process %d, which the system names as its child but which is not part of this keploy run, so keploy cannot capture or mock its calls", launcher, c)

	h, got, serr := claim(map[int]uint64{launcher: launcherStart})
	require.NotNil(t, serr, "the launcher is the run's, the child it started is not")
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
	require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
	require.Equal(t, refused, serr.Message)
	require.Equal(t, []int{launcher, c}, h.asked, "the hooks are asked about the child itself")
	require.Equal(t, claimant{}, got, "the refused claim claims nothing")

	_, got, serr = claim(map[int]uint64{launcher: launcherStart, c: childStart + 1})
	require.NotNil(t, serr, "held under another creation time")
	require.Equal(t, refused, serr.Message)
	require.Equal(t, claimant{}, got)

	_, got, serr = claim(map[int]uint64{launcher: launcherStart, c: childStart})
	require.Nil(t, serr, "a child the hooks hold in the run is the run's")
	require.Equal(t, claimant{pid: c, start: childStart}, got)

	ending := child(t)
	a, _ := wireAgent(t, models.MODE_RECORD)
	a.config.Agent.ClientNSPID = uint32(root)
	h = &runMemberHooks{held: map[int]uint64{launcher: launcherStart}}
	a.Hooks = &exitingHooks{runMemberHooks: h, exit: ending}
	_, serr = a.ClaimInstance(context.Background(), models.InstanceReq{Key: "k", Child: ending.Process.Pid}, launcher)
	require.NotNil(t, serr, "the child exited while it was checked")
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
	require.Equal(t, fmt.Sprintf("process %d claimed this keploy run for process %d, which is not a running process it started", launcher, ending.Process.Pid), serr.Message)
	require.Equal(t, []int{launcher, ending.Process.Pid}, h.asked)
	a.provenMu.Lock()
	got = a.proven.claim
	a.provenMu.Unlock()
	require.Equal(t, claimant{}, got)
}

// exitingHooks are run-member hooks (runMemberHooks) that, asked about
// exit's process, end it first and reap it: a process that exits while the
// agent checks it.
type exitingHooks struct {
	*runMemberHooks
	exit *exec.Cmd
}

func (h *exitingHooks) HoldsInRun(pid int, start uint64) bool {
	if pid == h.exit.Process.Pid {
		_ = h.exit.Process.Kill()
		_, _ = h.exit.Process.Wait()
	}
	return h.runMemberHooks.HoldsInRun(pid, start)
}
