package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

const scopeHelperEnv = "KEPLOY_TEST_SCOPE_HELPER"

func TestScopePIDHelperProcess(t *testing.T) {
	if os.Getenv(scopeHelperEnv) != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

type scopedCall struct {
	pid   uint32
	start uint64
}

type fakeScoperHooks struct {
	coreAgent.Hooks
	active       bool
	scoped       []scopedCall
	err          error
	setInstances []uint32
	endedInsts   []uint32
}

func (f *fakeScoperHooks) ScopedPidsActive() bool { return f.active }

func (f *fakeScoperHooks) AddScopedPID(nsTgid uint32, startTicks uint64) error {
	f.scoped = append(f.scoped, scopedCall{pid: nsTgid, start: startTicks})
	return f.err
}

func (f *fakeScoperHooks) SetInstance(_ uint32, v uint32) error {
	f.setInstances = append(f.setInstances, v)
	return nil
}

var (
	_ coreAgent.PIDScoper       = (*fakeScoperHooks)(nil)
	_ coreAgent.InstanceStamper = (*fakeScoperHooks)(nil)
)

func (f *fakeScoperHooks) EndInstance(v uint32) (int, error) {
	f.endedInsts = append(f.endedInsts, v)
	return 1, nil
}

func (f *fakeScoperHooks) InstanceLost(uint32) (coreAgent.InstanceLoss, error) {
	return coreAgent.InstanceLoss{}, nil
}

func (f *fakeScoperHooks) InstanceRefusal() error { return nil }

func (f *fakeScoperHooks) PIDMapRefusals(context.Context) (models.PIDMapRefusals, error) {
	return models.PIDMapRefusals{}, nil
}

func spawnHelperChild(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestScopePIDHelperProcess$")
	cmd.Env = append(os.Environ(), scopeHelperEnv+"=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

func TestScopePIDChecksRunAncestryAndCallsHooks(t *testing.T) {
	self := os.Getpid()
	childPID := spawnHelperChild(t)
	cfg := &config.Config{
		CommandType: "native",
		Agent: config.Agent{
			ClientNSPID: uint32(self),
		},
	}

	t.Run("inactive hooks is a clean 200 no-op", func(t *testing.T) {
		hooks := &fakeScoperHooks{active: false}
		a := &Agent{logger: zap.NewNop(), config: &config.Config{CommandType: "native"}, Hooks: hooks}
		scoped, serr := a.ScopePID(context.Background(), childPID)
		require.Nil(t, serr)
		require.False(t, scoped)
		require.Empty(t, hooks.scoped)
	})

	if runtime.GOOS != "linux" {
		return
	}

	t.Run("active hooks receive caller PID and start ticks", func(t *testing.T) {
		hooks := &fakeScoperHooks{active: true}
		a := &Agent{logger: zap.NewNop(), config: cfg, Hooks: hooks}
		scoped, serr := a.ScopePID(context.Background(), childPID)
		require.Nil(t, serr)
		require.True(t, scoped)
		require.Len(t, hooks.scoped, 1)
		require.Equal(t, uint32(childPID), hooks.scoped[0].pid)
		require.NotZero(t, hooks.scoped[0].start)
	})

	t.Run("hook error fails closed with 500 internal", func(t *testing.T) {
		hooks := &fakeScoperHooks{active: true, err: errors.New("map full")}
		a := &Agent{logger: zap.NewNop(), config: cfg, Hooks: hooks}
		scoped, serr := a.ScopePID(context.Background(), childPID)
		require.False(t, scoped)
		require.NotNil(t, serr)
		require.Equal(t, http.StatusInternalServerError, serr.Status)
		require.Equal(t, models.ScopeErrInternal, serr.Code)
		require.Contains(t, serr.Message, "map full")
	})

	t.Run("agent reading itself as caller is rejected with 500", func(t *testing.T) {
		was := agentPID
		agentPID = self
		t.Cleanup(func() { agentPID = was })
		hooks := &fakeScoperHooks{active: true}
		a := &Agent{logger: zap.NewNop(), config: cfg, Hooks: hooks}
		scoped, serr := a.ScopePID(context.Background(), self)
		require.False(t, scoped)
		require.NotNil(t, serr)
		require.Equal(t, http.StatusInternalServerError, serr.Status)
		require.Empty(t, hooks.scoped)
	})

	t.Run("process outside keploy run is rejected with 422", func(t *testing.T) {
		hooks := &fakeScoperHooks{active: true}
		a := &Agent{logger: zap.NewNop(), config: cfg, Hooks: hooks}
		scoped, serr := a.ScopePID(context.Background(), 1)
		require.False(t, scoped)
		require.NotNil(t, serr)
		require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
		require.Equal(t, models.ScopeErrNotInRun, serr.Code)
		require.Empty(t, hooks.scoped)
	})

	t.Run("ClaimInstance single-worker adds claimant and fails closed on hook error", func(t *testing.T) {
		hooks := &fakeScoperHooks{active: true}
		a := &Agent{logger: zap.NewNop(), config: cfg, Hooks: hooks}
		resp, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "orders"}, childPID)
		require.Nil(t, serr)
		require.Equal(t, models.InstanceRun, resp.Instance)
		require.Len(t, hooks.scoped, 1)
		require.Equal(t, uint32(childPID), hooks.scoped[0].pid)
		require.NotZero(t, hooks.scoped[0].start)

		hooksFail := &fakeScoperHooks{active: true, err: errors.New("scoped map full")}
		aFail := &Agent{logger: zap.NewNop(), config: cfg, Hooks: hooksFail}
		_, serr = aFail.ClaimInstance(context.Background(), models.InstanceReq{Key: "orders"}, childPID)
		require.NotNil(t, serr)
		require.Equal(t, http.StatusInternalServerError, serr.Status)
	})

	t.Run("ClaimInstance multi-worker adds worker and rolls back instance on hook error", func(t *testing.T) {
		wasMax := MaxWorkers
		MaxWorkers = func() int { return 4 }
		t.Cleanup(func() { MaxWorkers = wasMax })
		hooks := &fakeScoperHooks{active: true}
		a := &Agent{logger: zap.NewNop(), config: cfg, Hooks: hooks}
		resp, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "orders", Workers: 2}, childPID)
		require.Nil(t, serr)
		require.NotEmpty(t, resp.Instance)
		require.Len(t, hooks.scoped, 1)
		require.Equal(t, uint32(childPID), hooks.scoped[0].pid)
		require.NotZero(t, hooks.scoped[0].start)

		hooksFail := &fakeScoperHooks{active: true, err: errors.New("scoped map full")}
		aFail := &Agent{logger: zap.NewNop(), config: cfg, Hooks: hooksFail}
		_, serr = aFail.ClaimInstance(context.Background(), models.InstanceReq{Key: "orders", Workers: 2}, childPID)
		require.NotNil(t, serr)
		require.Equal(t, http.StatusInternalServerError, serr.Status)
		require.NotEmpty(t, hooksFail.setInstances)
		require.Equal(t, hooksFail.setInstances, hooksFail.endedInsts, "failed AddScopedPID must roll back the worker instance")
	})
}
