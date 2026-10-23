//go:build !linux

package tls

import (
	"context"
	"os/exec"

	"go.uber.org/zap"
)

// Off Linux the trust-store lock is a no-op, as in draft keploy#4755:
//
//   - macOS: keploy's one edit of a JDK keystore there is the retired CA's
//     removal, and every keploy that makes it makes the same one. Each edit
//     is made on a copy and renamed into place (editJavaKeystore), so two of
//     them at once leave the same keystore one would. And the agent runs
//     unprivileged there: a lock file it could open, any local user could
//     open and hold, keeping keploy from starting.
//   - Windows: an agent's start imports its run's CA and an unpersisted CA's
//     teardown deletes it (the persisted per-user CA, the default, is imported
//     once and kept), so two agents at once can lose one's edit, as on Linux
//     before the lock. Any user can create files in %ProgramData%, so the
//     lock needs one only Administrators can hold; that is not built yet.

// trustStoreLockIsMachineWide is false: lockTrustStore keeps nothing out here.
const trustStoreLockIsMachineWide = false

// lockTrustStore is a no-op off Linux. The work context is still not cancelled
// with ctx, as on Linux.
func lockTrustStore(ctx context.Context, _ *zap.Logger) (context.Context, func(), error) {
	return context.WithoutCancel(ctx), func() {}, nil
}

// noteTrustStoreProgress has no waiters to tell off Linux.
func noteTrustStoreProgress(context.Context) {}

// noteCommandProgress has no waiters to tell off Linux.
func noteCommandProgress(*exec.Cmd) (stop func()) { return func() {} }

// abandonedStoreCommand has no later wait to tell off Linux.
func abandonedStoreCommand(*exec.Cmd) {}
