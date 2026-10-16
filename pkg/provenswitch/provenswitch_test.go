package provenswitch

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// unpin undoes Pin for the rest of the test. Pin is one-way by design (an
// agent never unpins), so only this package's own tests may reach the state.
func unpin(t *testing.T) {
	t.Helper()
	pinned.Store(unpinned)
	t.Cleanup(func() { pinned.Store(unpinned) })
}

func TestOnReadsTheVariable(t *testing.T) {
	unpin(t)
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"1", true},
		{" 1 ", true},
		{"true", true},
		{"TRUE", true},
		// Not read at all: off here, and refused at startup by Validate.
		{"yes", false},
		{"on", false},
		{"ture", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(Env, tc.value)
			if got := On(); got != tc.want {
				t.Fatalf("%s=%q: On() = %v, want %v", Env, tc.value, got, tc.want)
			}
		})
	}
}

// A pinned process (an agent) answers with what was pinned, on or off, and
// never with its own environment: the CLI's flag is the only thing that
// decides an agent's switch.
func TestPinIgnoresTheVariable(t *testing.T) {
	unpin(t)
	t.Setenv(Env, "")
	Pin(true)
	if !On() || State() != "on" {
		t.Fatal("Pin(true) with KEPLOY_PROVEN unset: the agent must run the new paths the CLI asked for")
	}
	unpin(t)
	t.Setenv(Env, "1")
	Pin(false)
	if On() || State() != "off" {
		t.Fatal("Pin(false) with KEPLOY_PROVEN=1 in the agent's environment: the agent must not run paths its CLI did not ask for")
	}
}

func TestAgentArgs(t *testing.T) {
	unpin(t)
	t.Setenv(Env, "")
	if got := AgentArgs(); got != nil {
		t.Fatalf("switch off: AgentArgs() = %q, want nil so the agent argv is byte-identical to before", got)
	}
	t.Setenv(Env, "1")
	if got, want := AgentArgs(), []string{"--proven"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("switch on: AgentArgs() = %q, want %q", got, want)
	}
}

// A mistyped value must not quietly mean "off".
func TestValidateRefusesAValueItCannotRead(t *testing.T) {
	unpin(t)
	for _, ok := range []string{"", " ", "1", "0", "true", "false"} {
		t.Setenv(Env, ok)
		if err := Validate(); err != nil {
			t.Fatalf("%s=%q refused: %v", Env, ok, err)
		}
	}
	for _, bad := range []string{"yes", "on", "ture", "2"} {
		t.Setenv(Env, bad)
		err := Validate()
		if err == nil || !strings.Contains(err.Error(), Env) {
			t.Fatalf("%s=%q: Validate() = %v, want a refusal naming the variable", Env, bad, err)
		}
	}
}

func TestCheckAgent(t *testing.T) {
	unpin(t)
	for _, tc := range []struct {
		local, reported string
		ok              bool
	}{
		{"", "off", true},
		{"1", "on", true},
		{"", "", true},      // switch off, an agent that predates the switch: today's pairing
		{"1", "", false},    // switch on, an agent that cannot have it
		{"1", "off", false}, // an agent started without --proven (k8s-proxy today)
		{"", "on", false},   // an agent running paths this keploy does not
	} {
		t.Setenv(Env, tc.local)
		err := CheckAgent(tc.reported)
		if (err == nil) != tc.ok {
			t.Fatalf("local %q, agent %q: CheckAgent = %v, want ok=%v", tc.local, tc.reported, err, tc.ok)
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrAgentSwitch) {
			t.Fatalf("the refusal must wrap ErrAgentSwitch, so every caller can make it fatal: %v", err)
		}
		local := State()
		if !strings.Contains(err.Error(), local+" in this keploy") {
			t.Fatalf("the refusal must name this keploy's side (%s): %v", local, err)
		}
		if tc.reported != "" && !strings.Contains(err.Error(), tc.reported+" in its agent") {
			t.Fatalf("the refusal must name the agent's side (%s): %v", tc.reported, err)
		}
	}
}

// A pinned agent warns about a KEPLOY_PROVEN in its own environment only when
// it says something other than what was pinned.
func TestIgnoredEnv(t *testing.T) {
	unpin(t)
	t.Setenv(Env, "1")
	if _, ignored := IgnoredEnv(); ignored {
		t.Fatal("an unpinned process reads its variable; nothing is ignored")
	}
	for _, tc := range []struct {
		pin     bool
		value   string
		ignored bool
	}{
		{true, "1", false},  // the CLI ran as root and passed both: they agree
		{true, "", false},   // nothing set
		{false, "1", true},  // an agent started without --proven, the variable on
		{true, "0", true},   // the reverse
		{true, "yes", true}, // unreadable, and ignored
		{false, "false", false},
	} {
		unpin(t)
		Pin(tc.pin)
		t.Setenv(Env, tc.value)
		if v, ignored := IgnoredEnv(); ignored != tc.ignored || (ignored && v != tc.value) {
			t.Fatalf("pin %v, %s=%q: IgnoredEnv = (%q, %v), want ignored=%v", tc.pin, Env, tc.value, v, ignored, tc.ignored)
		}
	}
}

// Unpin hands the switch back to KEPLOY_PROVEN.
func TestUnpin(t *testing.T) {
	t.Cleanup(Unpin)
	t.Setenv(Env, "1")
	Pin(false)
	if On() {
		t.Fatal("pinned off, the switch read the environment")
	}
	Unpin()
	if !On() {
		t.Fatal("unpinned, the switch did not read KEPLOY_PROVEN=1")
	}
}
