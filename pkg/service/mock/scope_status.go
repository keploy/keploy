package mock

import (
	"context"
	"fmt"
	"os"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// ScopeStatusReader is an optional Instrumentation extension implemented by
// AgentClient: queries GET /agent/scope-status after the agent starts so the
// CLI can detect when cgroup-based Go toolchain scoping fell back to full-tree
// interception (scoped_ready=false).
type ScopeStatusReader interface {
	ScopeStatus(ctx context.Context) (models.ScopeStatus, error)
}

const defaultScopeFallbackRemedy = "run with sudo (CAP_SYS_ADMIN) or under a delegated systemd user scope"

// checkScopeStatus queries the agent's Go toolchain scoping state before the
// test command starts. When scoping was requested and could not be armed
// (scoped_ready=false, e.g. read-only /sys/fs/cgroup or a non-delegated
// session without CAP_SYS_ADMIN), it unsets KEPLOY_SCOPE_PID so keploy-go-exec
// stays on full-tree interception without failing its launch, and prints one
// clear warning line naming the cause and remedy.
func (m *mockService) checkScopeStatus(ctx context.Context) models.ScopeStatus {
	reader, ok := m.instrumentation.(ScopeStatusReader)
	if !ok {
		return models.ScopeStatus{}
	}
	st, err := reader.ScopeStatus(ctx)
	if err != nil {
		m.logger.Debug("could not read agent scope status", zap.Error(err))
		return models.ScopeStatus{}
	}
	if !st.Requested {
		return st
	}
	if !st.Ready {
		_ = os.Unsetenv("KEPLOY_SCOPE_PID")
		remedy := st.Remedy
		if remedy == "" {
			remedy = defaultScopeFallbackRemedy
		}
		reason := st.Reason
		if reason == "" {
			reason = "scoped cgroup unavailable"
		}
		m.logger.Warn("go toolchain exemption unavailable (scoped_ready=false): falling back to full-tree interception — Go module downloads may be mocked or blocked",
			zap.String("reason", reason),
			zap.String("remedy", remedy))
		if m.console != nil {
			_, _ = fmt.Fprintf(m.console, "WARN  go toolchain exemption unavailable (scoped_ready=false: %s): falling back to full-tree interception — Go module downloads may be mocked or blocked; %s\n", reason, remedy)
		}
	}
	return st
}

func scopedReadyPtr(st models.ScopeStatus) *bool {
	if !st.Requested {
		return nil
	}
	v := st.Ready
	return &v
}
