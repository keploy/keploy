package app

import (
	"testing"

	"github.com/docker/compose/v2/pkg/api"
)

// The CLI treats --exit-code-from as implying --abort-on-container-exit, and
// the library path claims to match the CLI exactly. Without the implication a
// command carrying only --exit-code-from leaves Up blocking after the app
// exits, where the shell-out would have returned.
func TestComposeUpSemanticsExitCodeFromImpliesAbort(t *testing.T) {
	got := composeUpSemantics("docker compose -f - up --exit-code-from app")
	if got.ExitCodeFrom != "app" {
		t.Fatalf("ExitCodeFrom = %q, want app", got.ExitCodeFrom)
	}
	if got.OnExit != api.CascadeStop {
		t.Fatalf("OnExit = %v, want CascadeStop: --exit-code-from implies --abort-on-container-exit", got.OnExit)
	}
}

// --abort-on-container-failure is the documented exception and must not be
// downgraded to CascadeStop by the implication above.
func TestComposeUpSemanticsAbortOnFailureKeepsPrecedence(t *testing.T) {
	got := composeUpSemantics("docker compose -f - up --abort-on-container-failure --exit-code-from app")
	if got.OnExit != api.CascadeFail {
		t.Fatalf("OnExit = %v, want CascadeFail", got.OnExit)
	}
}

// No --exit-code-from, no implication: a plain `up` still returns immediately.
func TestComposeUpSemanticsPlainUpIsUnchanged(t *testing.T) {
	got := composeUpSemantics("docker compose -f - up")
	if got.OnExit != api.CascadeIgnore {
		t.Fatalf("OnExit = %v, want CascadeIgnore for a plain up", got.OnExit)
	}
}
