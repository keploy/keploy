package replay

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// keploy test hands the agent, on the set's staging, every test case of the
// set in the order its run takes them up (models.ExactServing.Order):
// recorded order, with the streaming test cases (run in a pass after the
// rest) last, and with the ones it does not run among them (a test case a
// selection leaves out, a quarantined one), so the agent reaches a
// worker-scope mock's place once the run passes its test case, skipped or
// not. A set whose worker scope places no mock after a test case is staged
// with none: the agent reads the order for nothing else.
func TestTheStagingHandsTheAgentTheRunsOrderOfEveryTestCase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		worker []models.MockEntry
		want   []string
	}{
		{"a worker mock placed after a test case",
			[]models.MockEntry{{Name: "mock-boot", Kind: string(models.HTTP)}, {Name: "mock-late", Kind: string(models.HTTP), Place: "test-1"}},
			[]string{"test-1", "test-3", "test-4", "test-2"}},
		{"no worker mock placed after a test case",
			[]models.MockEntry{{Name: "mock-boot", Kind: string(models.HTTP)}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 4, 0)
			pinWindows(h)
			h.streaming("test-2")
			h.replayer.config.Command = "./app"
			// test-1 is quarantined, test-3 left out by a selection.
			h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {"test-1", "test-2", "test-4"}}
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1"}}
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010), httpAt("mock-late", 1050)}
			s12MappingFile(t, h, nil, tc.worker...)
			h.run(t)
			ex, ok := stagedExact(h)
			if !ok {
				t.Fatal("the set was not staged")
			}
			if !reflect.DeepEqual(ex.Order, tc.want) {
				t.Fatalf("the staging handed the agent the run's order %v; want %v", ex.Order, tc.want)
			}
		})
	}
}

// passOrder keeps every test case, in recorded order, the HTTP streaming ones
// last in their own recorded order; stagedOrder hands it on only when a mock
// of the worker scope the agent stages is placed after a test case.
func TestPassOrderAndWhenTheStagingHandsItOn(t *testing.T) {
	plain := func(name string) *models.TestCase {
		return &models.TestCase{Name: name, Kind: models.HTTP, HTTPResp: models.HTTPResp{StatusCode: 200}}
	}
	stream := func(name string) *models.TestCase {
		tc := plain(name)
		tc.HTTPResp.Header = map[string]string{"Content-Type": "application/x-ndjson"}
		return tc
	}
	// A gRPC test case is never deferred, whatever its HTTP response says.
	grpc := stream("g-5")
	grpc.Kind = models.GRPC_EXPORT
	got := passOrder([]*models.TestCase{stream("s-1"), plain("t-2"), stream("s-3"), plain("t-4"), grpc})
	if want := []string{"t-2", "t-4", "g-5", "s-1", "s-3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("passOrder = %v; want %v", got, want)
	}
	order := []string{"t-1", "t-2"}
	for _, tc := range []struct {
		name string
		plan *exactPlan
		want []string
	}{
		{"no plan", nil, nil},
		{"no worker scope", &exactPlan{order: order}, nil},
		{"a worker scope at the set's start only", &exactPlan{order: order, worker: []models.MockEntry{{Name: "w"}}}, nil},
		{"a placed mock the agent does not hold", &exactPlan{order: order, worker: []models.MockEntry{{Name: "w"}, {Name: "p", Place: "t-1"}}, unheld: map[string]bool{"p": true}}, nil},
		{"a placed mock the agent holds", &exactPlan{order: order, worker: []models.MockEntry{{Name: "w"}, {Name: "p", Place: "t-1"}}}, order},
	} {
		if got := tc.plan.stagedOrder(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: stagedOrder = %v; want %v", tc.name, got, tc.want)
		}
	}
}
