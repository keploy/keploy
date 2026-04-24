package util

import (
	"sync"
	"testing"
)

func TestNewIsNotTripped(t *testing.T) {
	ks := New()
	if ks.Enabled() {
		t.Fatalf("fresh KillSwitch should not be tripped")
	}
}

func TestTripAndReset(t *testing.T) {
	ks := New()

	ks.Trip()
	if !ks.Enabled() {
		t.Fatalf("after Trip, Enabled should be true")
	}

	ks.Reset()
	if ks.Enabled() {
		t.Fatalf("after Reset, Enabled should be false")
	}

	// Trip → Trip is idempotent.
	ks.Trip()
	ks.Trip()
	if !ks.Enabled() {
		t.Fatalf("after double Trip, Enabled should still be true")
	}
}

func TestNewFromEnv_Default(t *testing.T) {
	t.Setenv(envDisableParsing, "")
	ks := NewFromEnv()
	if ks.Enabled() {
		t.Fatalf("with env unset, KillSwitch should not be tripped")
	}
}

func TestNewFromEnv_Truthy(t *testing.T) {
	cases := []string{
		"1",
		"true",
		"TRUE",
		"True",
		"yes",
		"YES",
		"Yes",
		"  true  ", // surrounding whitespace still recognised
	}
	for _, v := range cases {
		v := v
		t.Run(v, func(t *testing.T) {
			t.Setenv(envDisableParsing, v)
			ks := NewFromEnv()
			if !ks.Enabled() {
				t.Fatalf("KEPLOY_DISABLE_PARSING=%q: expected Enabled==true", v)
			}
		})
	}
}

func TestNewFromEnv_Falsy(t *testing.T) {
	cases := []string{"0", "false", "FALSE", "no", "NO", "", "bogus", "enable"}
	for _, v := range cases {
		v := v
		t.Run(v, func(t *testing.T) {
			t.Setenv(envDisableParsing, v)
			ks := NewFromEnv()
			if ks.Enabled() {
				t.Fatalf("KEPLOY_DISABLE_PARSING=%q: expected Enabled==false", v)
			}
		})
	}
}

func TestConcurrentTripReset(t *testing.T) {
	ks := New()
	const workers = 64
	const iters = 1000

	var wg sync.WaitGroup
	wg.Add(workers * 2)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				ks.Trip()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				ks.Reset()
			}
		}()
	}

	wg.Wait()

	// End-state is intentionally unspecified (race between last
	// Trip and last Reset); what we assert is that the calls did
	// not crash and the value is a legal bool. A subsequent Trip
	// should deterministically set it.
	ks.Trip()
	if !ks.Enabled() {
		t.Fatalf("after concurrent hammering + final Trip, Enabled should be true")
	}
	ks.Reset()
	if ks.Enabled() {
		t.Fatalf("after concurrent hammering + final Reset, Enabled should be false")
	}
}

// TestDefaultKillSwitch_Exists smoke-checks that the package init
// produced a non-nil DefaultKillSwitch. Its tripped state depends
// on the environment the test binary was launched in, so we only
// assert identity, not value.
func TestDefaultKillSwitch_Exists(t *testing.T) {
	if DefaultKillSwitch == nil {
		t.Fatalf("DefaultKillSwitch must be non-nil")
	}
}

// TestIsTruthy covers the helper directly so future refactors of
// env-var parsing don't regress the accepted set.
func TestIsTruthy(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "True", "yes", "YES", " 1 ", "\ttrue\n"}
	for _, s := range truthy {
		if !isTruthy(s) {
			t.Errorf("isTruthy(%q) = false, want true", s)
		}
	}
	falsy := []string{"", " ", "0", "false", "no", "maybe", "enable", "2"}
	for _, s := range falsy {
		if isTruthy(s) {
			t.Errorf("isTruthy(%q) = true, want false", s)
		}
	}
}
