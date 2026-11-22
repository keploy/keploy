package replay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
)

// TestRunTestSetLeavesARecordedJSONMappingAlone: without --update-test-mapping
// a replay writes a test set's mappings file only when the set has none, and
// adds at most a startup section to one it has. A set recorded with
// --storage-format json has mappings.json. Through the real mapping store, a
// run of such a set, in either storage format, must leave the recorded test
// entries of that file as they are: replacing each test's recorded mocks with
// the ones this run consumed (here a session mock every test shares) changes
// which mocks a later mapping-based replay, strict or not, gives each test.
func TestRunTestSetLeavesARecordedJSONMappingAlone(t *testing.T) {
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format)+" replay", func(t *testing.T) {
			h := newPartialRunHarness(t, 4, 0)
			h.replayer.hookImpl = prHooks{consumed: session}
			dir := h.replayer.config.Path

			recorded := make(map[string][]models.MockEntry, 4)
			for i := 1; i <= 4; i++ {
				recorded[fmt.Sprintf("test-%d", i)] = []models.MockEntry{{Name: fmt.Sprintf("mock-own-%d", i), Kind: string(models.HTTP)}}
			}
			if err := mapdb.NewWithFormat(zap.NewNop(), dir, "mappings", yaml.FormatJSON).
				UpsertBatch(context.Background(), "test-set-0", recorded); err != nil {
				t.Fatalf("recording the mappings: %v", err)
			}
			jsonFile := filepath.Join(dir, "test-set-0", "mappings.json")
			if _, err := os.Stat(jsonFile); err != nil {
				t.Fatalf("precondition: no mappings.json was recorded: %v", err)
			}

			h.replayer.mappingDB = mapdb.NewWithFormat(zap.NewNop(), dir, "mappings", format)
			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("precondition: want a passing run, got %q", status)
			}

			if _, err := os.Stat(filepath.Join(dir, "test-set-0", "mappings.yaml")); !os.IsNotExist(err) {
				t.Fatalf("the run wrote a mappings.yaml beside the recorded mappings.json (stat err: %v)", err)
			}
			reader := mapdb.NewWithFormat(zap.NewNop(), dir, "mappings", yaml.FormatJSON)
			after, _, err := reader.Get(context.Background(), "test-set-0")
			if err != nil {
				t.Fatalf("reading mappings.json after the run: %v", err)
			}
			if !reflect.DeepEqual(after, recorded) {
				raw, _ := os.ReadFile(jsonFile)
				t.Fatalf("a default %s replay replaced the recorded test entries of mappings.json\nrecorded: %v\nafter the run:\n%s", format, recorded, raw)
			}
			// The one change such a run may make: the startup section it captured,
			// added to a file that had none (backfillStartupSection). Checking it
			// shows the run reached the mapping write at all.
			startup, err := reader.GetStartup(context.Background(), "test-set-0")
			if err != nil || len(startup) != 1 || startup[0].Name != "mock-session" {
				t.Fatalf("control: want the run to add its startup section {mock-session}, got %+v (err %v)", startup, err)
			}
		})
	}
}
