package schemanoise_test

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
)

// The deprecated path only stays usable if its types are ALIASES rather than
// distinct definitions: a module still importing schemanoise has to be able to
// hand values to, and receive them from, code that has already moved to
// mocknoise. These assignments do not compile unless the two names denote the
// same type, so this file is the compile-time guard for that.
var (
	_ *mocknoise.Engine      = (*schemanoise.Engine)(nil)
	_ *schemanoise.Engine    = (*mocknoise.Engine)(nil)
	_ mocknoise.Adapter      = (schemanoise.Adapter)(nil)
	_ schemanoise.Adapter    = (mocknoise.Adapter)(nil)
	_ mocknoise.JSONDiffer   = schemanoise.JSONDiffer{}
	_ schemanoise.JSONDiffer = mocknoise.JSONDiffer{}
)

// stubAdapter is a minimal Adapter declared against the DEPRECATED package.
// That it satisfies mocknoise.Adapter too is itself part of what is asserted:
// a consumer that implemented the interface before the rename must not have to
// change to keep working.
type stubAdapter struct{ schemanoise.JSONDiffer }

func (stubAdapter) RecordedBody(*models.Mock) ([]byte, bool)            { return []byte(`{"a":1}`), true }
func (stubAdapter) StoredNoise(*models.Mock) map[string][]string        { return nil }
func (stubAdapter) SetLearnedNoise(*models.Mock, map[string][]string)   {}
func (stubAdapter) RecordedValueIsNoise(*models.Mock) func(string) bool { return nil }

var _ mocknoise.Adapter = stubAdapter{}

func TestDeprecatedPathForwardsToMocknoise(t *testing.T) {
	// An engine built through the old constructor must behave as one built
	// through the new one — i.e. the shim forwards rather than re-implements.
	old := schemanoise.New(stubAdapter{}, true, false)
	if !old.DetectionEnabled() || old.StrictEnabled() {
		t.Fatalf("flags not forwarded: detection=%v strict=%v",
			old.DetectionEnabled(), old.StrictEnabled())
	}

	// A value from the old path must be usable where the new type is wanted;
	// this is the property the alias buys and a wrapper type would not.
	var asNew *mocknoise.Engine = old
	if !asNew.DetectionEnabled() {
		t.Fatal("engine did not survive the crossing to mocknoise.Engine")
	}
}

func TestDeprecatedHelpersForward(t *testing.T) {
	in := map[string][]string{"user.id": nil}

	if got, want := schemanoise.AddBodyPrefix(in), mocknoise.AddBodyPrefix(in); len(got) != len(want) {
		t.Fatalf("AddBodyPrefix diverged: %v vs %v", got, want)
	}
	prefixed := mocknoise.AddBodyPrefix(in)
	if got, want := schemanoise.StripBodyPrefix(prefixed), mocknoise.StripBodyPrefix(prefixed); len(got) != len(want) {
		t.Fatalf("StripBodyPrefix diverged: %v vs %v", got, want)
	}
	if got, want := schemanoise.MergeKnown(in, in), mocknoise.MergeKnown(in, in); len(got) != len(want) {
		t.Fatalf("MergeKnown diverged: %v vs %v", got, want)
	}
	if got, want := schemanoise.MergeLearned(in, in), mocknoise.MergeLearned(in, in); len(got) != len(want) {
		t.Fatalf("MergeLearned diverged: %v vs %v", got, want)
	}

	_, comparable := schemanoise.DetectJSONDrift([]byte(`{"a":1}`), []byte(`{"a":2}`), nil, nil)
	if !comparable {
		t.Fatal("DetectJSONDrift did not forward: JSON payload reported as opaque")
	}
}
