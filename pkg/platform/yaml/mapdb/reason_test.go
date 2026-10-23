package mapdb

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Each move's Reason is written on the entry it moves or places as given: a
// move without one leaves the entry none.
func TestMoveToWorkerWritesEachMovesReason(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(format.FileExtension(), func(t *testing.T) {
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
			m := goldenMapping()
			m.Worker[0].Reason = "mock-2 (SQS ReceiveMessage) was recorded before the first test. Undo: delete this line."
			m.Worker[1].Reason = "routed at record"
			if err := db.Insert(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			moved := "mock-6 (Mongo insert keploy.events) moved from post-url-1 to background. Undo: move this entry back under tests[post-url-1].mocks and delete this line."
			if _, err := db.MoveToWorker(context.Background(), "test-set-0", []models.MockEntry{
				{Name: "mock-6", Reason: moved}, // a test's entry moves, with its line
				{Name: "mock-2", Place: ""},     // a worker entry is placed, with no line
			}); err != nil {
				t.Fatal(err)
			}
			got, err := db.GetMapping(context.Background(), "test-set-0")
			if err != nil {
				t.Fatal(err)
			}
			reasons := map[string]string{}
			for _, e := range got.Worker {
				reasons[e.Name] = e.Reason
			}
			if reasons["mock-6"] != moved {
				t.Errorf("mock-6's reason is %q; want the move's", reasons["mock-6"])
			}
			if reasons["mock-2"] != "" {
				t.Errorf("mock-2's reason is %q; a move without one leaves none", reasons["mock-2"])
			}
			if reasons["mock-5"] != "routed at record" {
				t.Errorf("mock-5, moved by nothing, has the reason %q; want its own", reasons["mock-5"])
			}
		})
	}
}

// A rewrite of mappings.yaml without the proven switch keeps every Reason a
// run with it wrote: each write decodes the file whole
// and encodes it back, so a line on an entry the write does not touch stays,
// on a worker entry and on a test's alike.
func TestAReasonSurvivesARewriteWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	routed := "mock-5 (SQS DeleteMessage) was sent before post-url-1 began and answered inside it, by one clock. Undo: move this entry under tests[post-url-1].mocks and delete this line."
	owned := "kept on its test by hand. Undo: delete this line."
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(format.FileExtension(), func(t *testing.T) {
			ctx := context.Background()
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
			m := goldenMapping()
			m.Worker[1].Reason = routed
			m.TestCases[1].Mocks[1].Reason = owned
			if err := db.Insert(ctx, m); err != nil {
				t.Fatal(err)
			}
			// What keploy record and keploy test write without the switch.
			if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
				"post-url-1": {{Name: "mock-8", Kind: "Mongo"}}, // merged into a test with a line
				"post-url-2": {{Name: "mock-9", Kind: "Mongo"}}, // a new test
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertBoots(ctx, "test-set-0", []models.BootSpec{{Key: "app", Mocks: []models.MockEntry{}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{{Name: "mock-3"}}); err != nil {
				t.Fatal(err)
			}
			got, err := db.GetMapping(ctx, "test-set-0")
			if err != nil {
				t.Fatal(err)
			}
			reasons := map[string]string{}
			for _, e := range got.Worker {
				reasons[e.Name] = e.Reason
			}
			for _, tc := range got.TestCases {
				for _, e := range tc.Mocks {
					reasons[e.Name] = e.Reason
				}
			}
			if reasons["mock-5"] != routed {
				t.Errorf("the worker entry mock-5 has the line %q after the rewrites; want its own", reasons["mock-5"])
			}
			if reasons["mock-7"] != owned {
				t.Errorf("post-url-1's mock-7 has the line %q after the rewrites; want its own", reasons["mock-7"])
			}
			for _, name := range []string{"mock-2", "mock-3", "mock-6", "mock-8", "mock-9"} {
				if reasons[name] != "" {
					t.Errorf("%s, given no line, has %q", name, reasons[name])
				}
			}
		})
	}
}
