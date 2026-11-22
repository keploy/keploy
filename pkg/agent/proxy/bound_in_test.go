package proxy

import (
	"context"
	"slices"
	"testing"

	"go.uber.org/zap"
)

// The agent's read of a test's misses keeps where a variable miss's
// variables were bound (MockMismatchReport.BoundIn), in a slice of its own:
// the replay's judge reads it to tell which recordings shaped the refusal.
func TestTheMissReadKeepsWhereAVariableMissWasBound(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	p.BeginTestErrorCapture()
	miss := mockMiss("findAndModify entity_versions")
	miss.MismatchReport.BoundIn = []string{"mock-2065"}
	p.errChannel <- miss
	p.errChannel <- mockMiss("find users")
	got, err := p.GetMockErrors(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("read %+v, %v; want both misses", got, err)
	}
	for _, c := range got {
		want := []string(nil)
		if c.ActualSummary == "findAndModify entity_versions" {
			want = []string{"mock-2065"}
		}
		if !slices.Equal(c.BoundIn, want) {
			t.Errorf("%s: BoundIn %v; want %v", c.ActualSummary, c.BoundIn, want)
		}
	}
	miss.MismatchReport.BoundIn[0] = "mock-1"
	for _, c := range got {
		if c.ActualSummary == "findAndModify entity_versions" && c.BoundIn[0] != "mock-2065" {
			t.Errorf("the read miss changed with the parser's report: %v", c.BoundIn)
		}
	}
}
