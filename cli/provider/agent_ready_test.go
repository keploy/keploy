package provider

import (
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A KEPLOY_AGENT_READY_TIMEOUT that is set is said to change nothing, with
// what the wait for the agent follows instead; one that is not set says
// nothing.
func TestARemovedAgentReadyTimeoutThatIsSetIsSaidToChangeNothing(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	t.Setenv(removedAgentReadyTimeout, "600")
	warnRemovedAgentReadyTimeout(zap.New(core))
	if logs.Len() != 1 {
		t.Fatalf("%d warnings, want 1", logs.Len())
	}
	e := logs.All()[0]
	if !strings.Contains(e.Message, "KEPLOY_AGENT_READY_TIMEOUT is no longer read") || !strings.Contains(e.Message, "makes progress") || e.ContextMap()["value"] != "600" {
		t.Fatalf("the warning says %q %v; want the variable, that it changes nothing, what the wait follows, and its value", e.Message, e.ContextMap())
	}

	core, logs = observer.New(zapcore.WarnLevel)
	// Set first, so that the test's end puts back what was there.
	t.Setenv(removedAgentReadyTimeout, "")
	if err := os.Unsetenv(removedAgentReadyTimeout); err != nil {
		t.Fatal(err)
	}
	warnRemovedAgentReadyTimeout(zap.New(core))
	if logs.Len() != 0 {
		t.Fatalf("a variable that is not set was warned of: %q", logs.All()[0].Message)
	}
}

// keploy's flag handling warns of it, for a command that waits for an agent
// (keploy test here), and only once.
func TestKeploysFlagHandlingWarnsOfARemovedAgentReadyTimeout(t *testing.T) {
	t.Setenv(removedAgentReadyTimeout, "600")
	_, logs, err := deletedRun(t, "")
	if err != nil {
		t.Fatalf("ValidateFlags: %v", err)
	}
	if n := logs.FilterMessageSnippet("KEPLOY_AGENT_READY_TIMEOUT is no longer read").Len(); n != 1 {
		t.Fatalf("%d warnings of the removed variable, want 1", n)
	}
}
