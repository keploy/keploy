package replay

import (
	"reflect"
	"testing"

	"facette.io/natsort"
)

func TestMoveRunLastTestSetsToEnd(t *testing.T) {
	tests := []struct {
		name     string
		testSets []string
		runLast  []string
		want     []string
	}{
		{
			name:     "historical set moves after recorded sets despite sorting first",
			testSets: []string{"__historical__smart-set", "test-set-0", "test-set-1"},
			runLast:  []string{"__historical__smart-set"},
			want:     []string{"test-set-0", "test-set-1", "__historical__smart-set"},
		},
		{
			name:     "empty runLast keeps order",
			testSets: []string{"__historical__smart-set", "test-set-0"},
			runLast:  nil,
			want:     []string{"__historical__smart-set", "test-set-0"},
		},
		{
			name:     "runLast id not present is ignored",
			testSets: []string{"test-set-0", "test-set-1"},
			runLast:  []string{"__historical__smart-set"},
			want:     []string{"test-set-0", "test-set-1"},
		},
		{
			name:     "multiple deferred sets keep their relative order",
			testSets: []string{"__historical__a", "__historical__b", "test-set-0"},
			runLast:  []string{"__historical__a", "__historical__b"},
			want:     []string{"test-set-0", "__historical__a", "__historical__b"},
		},
		{
			name:     "empty testSets",
			testSets: nil,
			runLast:  []string{"__historical__smart-set"},
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := moveRunLastTestSetsToEnd(tt.testSets, tt.runLast)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("moveRunLastTestSetsToEnd(%v, %v) = %v, want %v", tt.testSets, tt.runLast, got, tt.want)
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
	got := moveRunLastTestSetsToEnd(sets, []string{"__historical__smart-set"})
	if got[len(got)-1] != "__historical__smart-set" {
		t.Fatalf("expected __historical__smart-set last after reorder, got %v", got)
	}
}
