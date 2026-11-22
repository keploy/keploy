package schemanoise_test

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
)

// These assert the property the whole shim exists for: the two import paths
// name the SAME types, so a value built on one side is accepted on the other.
//
// The rename lands across keploy, integrations, enterprise, k8s-proxy and
// enterprise-ui, which merge in an arbitrary order. During that window a
// migrated caller and an unmigrated one will meet, and if these were defined
// types rather than aliases every such meeting would be a compile error in a
// repo that did nothing wrong. A defined type would still pass a test that only
// exercised one side, so the assignments below are deliberately cross-wise.
//
// They are compile-time assertions: if the aliases ever become defined types
// this file stops building, which is the loudest possible failure and exactly
// what we want for a compatibility guarantee.

func TestEngineTypeIsSharedAcrossBothImportPaths(t *testing.T) {
	// Built through the deprecated path, held as the canonical type.
	var canonical *mocknoise.Engine = schemanoise.New(stubAdapter{}, true, false)
	if canonical == nil {
		t.Fatal("schemanoise.New returned nil")
	}
	// ...and the reverse.
	var deprecated *schemanoise.Engine = mocknoise.New(stubAdapter{}, true, false)
	if !deprecated.DetectionEnabled() {
		t.Error("engine built via mocknoise lost its flags when held as schemanoise.Engine")
	}
}

func TestAdapterInterfaceIsSharedAcrossBothImportPaths(t *testing.T) {
	// An adapter written against either spelling must satisfy the other's
	// interface — this is what lets integrations migrate on its own schedule.
	var viaDeprecated schemanoise.Adapter = stubAdapter{}
	var viaCanonical mocknoise.Adapter = viaDeprecated
	if viaCanonical == nil {
		t.Fatal("adapter did not cross the alias boundary")
	}
}

func TestJSONDifferIsSharedAcrossBothImportPaths(t *testing.T) {
	var d schemanoise.JSONDiffer
	var _ mocknoise.JSONDiffer = d
}

// stubAdapter embeds the differ through the DEPRECATED path on purpose: an
// adapter in an unmigrated repo looks exactly like this, and it must still
// satisfy mocknoise.Adapter.
type stubAdapter struct {
	schemanoise.JSONDiffer
}

func (stubAdapter) RecordedBody(*models.Mock) ([]byte, bool)            { return nil, false }
func (stubAdapter) StoredNoise(*models.Mock) map[string][]string        { return nil }
func (stubAdapter) SetLearnedNoise(*models.Mock, map[string][]string)   {}
func (stubAdapter) RecordedValueIsNoise(*models.Mock) func(string) bool { return nil }
