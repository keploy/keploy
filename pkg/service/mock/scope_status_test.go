package mock

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type fakeScopeStatusInst struct {
	Instrumentation
	st  models.ScopeStatus
	err error
}

func (f fakeScopeStatusInst) ScopeStatus(context.Context) (models.ScopeStatus, error) {
	return f.st, f.err
}

func TestCheckScopeStatusFallbackIsLegibleAndUnsetsScopeEnv(t *testing.T) {
	t.Setenv("KEPLOY_SCOPE_PID", "1")
	var console bytes.Buffer
	m := &mockService{
		logger:  zap.NewNop(),
		console: &console,
		instrumentation: fakeScopeStatusInst{
			st: models.ScopeStatus{
				Requested: true,
				Ready:     false,
				Reason:    "create scoped cgroup /sys/fs/cgroup/keploy-scoped-1: read-only file system",
				Remedy:    "run with sudo (CAP_SYS_ADMIN) or under a delegated systemd user scope",
			},
		},
	}

	got := m.checkScopeStatus(context.Background())
	require.True(t, got.Requested)
	require.False(t, got.Ready)
	require.Empty(t, os.Getenv("KEPLOY_SCOPE_PID"), "fallback must unset KEPLOY_SCOPE_PID so keploy-go-exec runs under full-tree interception")
	require.Contains(t, console.String(), "scoped_ready=false")
	require.Contains(t, console.String(), "read-only file system")
	require.Contains(t, console.String(), "delegated systemd user scope")

	ptr := scopedReadyPtr(got)
	require.NotNil(t, ptr)
	require.False(t, *ptr)
}

func TestCheckScopeStatusReadyKeepsScopeEnv(t *testing.T) {
	t.Setenv("KEPLOY_SCOPE_PID", "1")
	var console bytes.Buffer
	m := &mockService{
		logger:  zap.NewNop(),
		console: &console,
		instrumentation: fakeScopeStatusInst{
			st: models.ScopeStatus{Requested: true, Ready: true},
		},
	}
	got := m.checkScopeStatus(context.Background())
	require.True(t, got.Requested)
	require.True(t, got.Ready)
	require.Equal(t, "1", os.Getenv("KEPLOY_SCOPE_PID"))
	require.Empty(t, console.String())
	ptr := scopedReadyPtr(got)
	require.NotNil(t, ptr)
	require.True(t, *ptr)
}
