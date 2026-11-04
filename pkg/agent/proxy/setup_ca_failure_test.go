package proxy

import (
	"fmt"
	"os"
	"strings"
	"testing"

	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The log of a failed CA install says what to fix. The native Linux install's
// store update that did not run because another process held the trust
// stores' lock past its bound (its error is pTls.ErrStoreLockHeld's, as the
// install returns it, here also wrapped once: errors.Is still finds it) is no
// missing write access: its next_step names the lock's file and says what to
// do with the process that holds it, which the error names. Any other
// failure keeps the write-access advice.
func TestSetupCAFailureNextStep(t *testing.T) {
	held := fmt.Errorf("%w: the lock was held for 2m0s by another keploy run on this host", pTls.ErrStoreLockHeld)
	for _, tc := range []struct {
		name string
		err  error
		want []string
		not  string
	}{
		{"the stores' lock stayed held", held, []string{pTls.StoreLockHeldNextStep(), "store.lock", "kill -CONT"}, "write access"},
		{"the stores' lock stayed held, wrapped", fmt.Errorf("install: %w", held), []string{pTls.StoreLockHeldNextStep()}, "write access"},
		{"another failure", fmt.Errorf("failed to update the CA store: %w", os.ErrPermission), []string{"write access"}, "store.lock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.ErrorLevel)
			logSetupCAFailure(zap.New(core), tc.err)
			entries := logs.FilterMessageSnippet("SetupCA failed").All()
			if len(entries) != 1 {
				t.Fatalf("logs %v, want the one that the CA's install failed", logs.All())
			}
			fields := entries[0].ContextMap()
			step, _ := fields["next_step"].(string)
			for _, w := range tc.want {
				if !strings.Contains(step, w) {
					t.Errorf("next_step %q does not say %q", step, w)
				}
			}
			if strings.Contains(step, tc.not) {
				t.Errorf("next_step %q says %q", step, tc.not)
			}
			if fields["error"] != tc.err.Error() {
				t.Errorf("the log's error %v, want %q", fields["error"], tc.err.Error())
			}
		})
	}
}
