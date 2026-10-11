package provenswitch

import (
	"reflect"
	"testing"
)

// resetEnable undoes Enable for the rest of the test. Enable is one-way by
// design (an agent never turns the switch off again), so only this package's
// own tests may reach the flag.
func resetEnable(t *testing.T) {
	t.Helper()
	enabled.Store(false)
	t.Cleanup(func() { enabled.Store(false) })
}

func TestOnReadsTheVariable(t *testing.T) {
	resetEnable(t)
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false}, // ParseBool does not read it: off, never guessed on
		{"2", false},
		{"1", true},
		{" 1 ", true},
		{"true", true},
		{"TRUE", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(Env, tc.value)
			if got := On(); got != tc.want {
				t.Fatalf("%s=%q: On() = %v, want %v", Env, tc.value, got, tc.want)
			}
		})
	}
}

func TestOnIsOffWithoutTheVariable(t *testing.T) {
	resetEnable(t)
	t.Setenv(Env, "")
	if On() {
		t.Fatal("On() is true with KEPLOY_PROVEN unset and no --proven: today's behaviour must stay the default")
	}
}

// The agent never sees KEPLOY_PROVEN (sudo drops it, the container is not
// given it): its --proven flag calls Enable, and On must then be true with the
// variable unset.
func TestEnableTurnsOnWithoutTheVariable(t *testing.T) {
	resetEnable(t)
	t.Setenv(Env, "")
	Enable()
	if !On() {
		t.Fatal("On() is false after Enable(): an agent started with --proven would run the old paths")
	}
	t.Setenv(Env, "0")
	if !On() {
		t.Fatal("On() is false after Enable() with KEPLOY_PROVEN=0: the flag the CLI passed must win over a stale variable in the agent's environment")
	}
}

func TestAgentArgs(t *testing.T) {
	resetEnable(t)
	t.Setenv(Env, "")
	if got := AgentArgs(); got != nil {
		t.Fatalf("switch off: AgentArgs() = %q, want nil so the agent argv is byte-identical to before", got)
	}
	t.Setenv(Env, "1")
	if got, want := AgentArgs(), []string{"--proven"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("switch on: AgentArgs() = %q, want %q", got, want)
	}
}
