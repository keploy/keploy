package replay

import (
	"reflect"
	"testing"

	"facette.io/natsort"
)

type fakeOrderingHooks struct {
	TestHooks
	order func(testSets []string) []string
}

func (f fakeOrderingHooks) OrderTestSets(testSets []string) []string {
	return f.order(testSets)
}

type plainHooks struct {
	TestHooks
}

func deferHistorical(testSets []string) []string {
	ordered := make([]string, 0, len(testSets))
	deferred := make([]string, 0, len(testSets))
	for _, id := range testSets {
		if id == "__historical__smart-set" {
			deferred = append(deferred, id)
			continue
		}
		ordered = append(ordered, id)
	}
	return append(ordered, deferred...)
}

func TestApplyTestSetOrder(t *testing.T) {
	tests := []struct {
		name     string
		hooks    TestHooks
		testSets []string
		want     []string
	}{
		{
			name:     "orderer moves historical set last despite sorting first",
			hooks:    fakeOrderingHooks{order: deferHistorical},
			testSets: []string{"__historical__smart-set", "test-set-0", "test-set-1"},
			want:     []string{"test-set-0", "test-set-1", "__historical__smart-set"},
		},
		{
			name:     "hooks without the extension keep the sorted order",
			hooks:    plainHooks{},
			testSets: []string{"__historical__smart-set", "test-set-0"},
			want:     []string{"__historical__smart-set", "test-set-0"},
		},
		{
			name:     "result of a different length is ignored",
			hooks:    fakeOrderingHooks{order: func([]string) []string { return []string{"test-set-0"} }},
			testSets: []string{"test-set-0", "test-set-1"},
			want:     []string{"test-set-0", "test-set-1"},
		},
		{
			name:     "empty input stays empty",
			hooks:    fakeOrderingHooks{order: deferHistorical},
			testSets: nil,
			want:     []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Replayer{hookImpl: tt.hooks}
			got := r.applyTestSetOrder(tt.testSets)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("applyTestSetOrder(%v) = %v, want %v", tt.testSets, got, tt.want)
			}
		})
	}
}

func TestNatsortPlacesHistoricalFirst(t *testing.T) {
	sets := []string{"test-set-55bcbdbbd6-api-server", "__historical__smart-set", "test-set-0"}
	natsort.Sort(sets)
	if sets[0] != "__historical__smart-set" {
		t.Fatalf("expected natsort to place __historical__smart-set first, got %v", sets)
	}
	r := &Replayer{hookImpl: fakeOrderingHooks{order: deferHistorical}}
	got := r.applyTestSetOrder(sets)
	if got[len(got)-1] != "__historical__smart-set" {
		t.Fatalf("expected __historical__smart-set last after reorder, got %v", got)
	}
}
