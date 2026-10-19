package agent

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// recordedFrom is a mock stamped as the keyer stamps one of a container's
// connections: key, instance and scope.
func recordedFromContainer(name string, kind models.Kind, key, inst, scope string) *models.Mock {
	return &models.Mock{Name: name, Kind: kind, Spec: models.MockSpec{Metadata: map[string]string{
		containerkey.MetadataKey: key, containerkey.InstanceKey: inst, containerkey.ScopeKey: scope,
	}}}
}

// Under the proven switch, storing a test set's mocks tells the keyer which
// recorded keys answered two containers in one scope: from the whole set, in
// either tier, through either path, a mock refused for its variables counted
// too (it is still a recording of its container).
func TestStoringASetsMocksTellsTheKeyerItsAmbiguousKeys(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Cleanup(func() { containerkey.Default.SetRecorded(&containerkey.RecordedTally{}) })
	const key = "container:postgres:5432@0123456789ab"
	set := func() ([]*models.Mock, []*models.Mock, *models.Mock) {
		a := recordedFromContainer("a", models.HTTP, key, "inst-a", "s2")
		// b, the other container of the scope, is refused for its variables
		// (no follower is registered for Mongo mocks).
		b := recordedFromContainer("b", models.Mongo, key, "inst-b", "s2")
		b.Spec.Variables = models.MockVariables{"id": {Recorded: "x", Use: []string{"body.id"}}}
		alone := recordedFromContainer("alone", models.HTTP, key, "inst-a", "s3")
		return []*models.Mock{a}, []*models.Mock{b, alone}, a
	}
	for _, path := range []string{"StoreMocks", "StoreMocksStream"} {
		t.Run(path, func(t *testing.T) {
			containerkey.Default.SetRecorded(&containerkey.RecordedTally{})
			filtered, unfiltered, a := set()
			ag := newTestAgent()
			if path == "StoreMocks" {
				if err := ag.StoreMocks(context.Background(), filtered, unfiltered); err != nil {
					t.Fatal(err)
				}
			} else {
				header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, filtered, unfiltered))
				if err := ag.StoreMocksStream(context.Background(), header, dec); err != nil {
					t.Fatal(err)
				}
			}
			if !containerkey.Default.RecordedAmbiguous(a) {
				t.Fatal("a key that answered two containers in one scope is not ambiguous")
			}
			alone := recordedFromContainer("alone", models.HTTP, key, "inst-a", "s3")
			if containerkey.Default.RecordedAmbiguous(alone) {
				t.Fatal("a key that answered one container in its scope is ambiguous")
			}
		})
	}
}
