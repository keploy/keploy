package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// followedManager is a store under exact serving with test-1 open, owning a
// recording named a and one named b.
func followedManager(t *testing.T) (*MockManager, []*models.Mock) {
	t.Helper()
	m := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(m.Close)
	own := []*models.Mock{{Name: "a"}, {Name: "b"}}
	m.ServeExactTest("test-1", own)
	return m, own
}

func TestAnExcusedRecordingLeavesItsTestsOutcome(t *testing.T) {
	m, _ := followedManager(t)
	m.ExcuseRecording("a")
	if _, _, unconsumed, ok := m.ExactTestOutcome(); !ok || len(unconsumed) != 1 || unconsumed[0] != "b" {
		t.Fatalf("outcome %v (ok %v): want b alone left", unconsumed, ok)
	}
}

func TestAnExcuseLastsForItsStaging(t *testing.T) {
	m, own := followedManager(t)
	m.ExcuseRecording("a")
	m.bumpStagingEpoch()
	m.ServeExactTest("test-1", own)
	if _, _, unconsumed, _ := m.ExactTestOutcome(); len(unconsumed) != 2 {
		t.Fatalf("outcome %v: a new staging excuses nothing", unconsumed)
	}
}

func TestATestOpenedAgainCountsOneCycle(t *testing.T) {
	m, own := followedManager(t)
	m.ServeExactTest("test-2", nil)
	if c := m.TestCycle(); c != 0 {
		t.Fatalf("two tests opened once each: cycle %d", c)
	}
	m.ServeExactTest("test-1", own)
	m.ServeExactTest("test-2", nil)
	if c := m.TestCycle(); c != 1 {
		t.Fatalf("the set run again: cycle %d, want 1", c)
	}
	m.ServeExactTest("test-1", own)
	if c := m.TestCycle(); c != 2 {
		t.Fatalf("the third run: cycle %d, want 2", c)
	}
	m.bumpStagingEpoch()
	if c := m.TestCycle(); c != 0 {
		t.Fatalf("a new staging: cycle %d, want 0", c)
	}
}

func TestRecordingsAreReadableOnlyOnceSaid(t *testing.T) {
	m := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(m.Close)
	m.bumpStagingEpoch()
	if m.RecordingsReadable() == m.stagingEpoch.Load() {
		t.Fatal("a staging is readable before the agent says so")
	}
	m.MarkRecordingsReadable()
	if m.RecordingsReadable() != m.stagingEpoch.Load() {
		t.Fatal("the epoch the agent said is not readable")
	}
	m.bumpStagingEpoch()
	if m.RecordingsReadable() == m.stagingEpoch.Load() {
		t.Fatal("the next staging is readable before the agent says so")
	}
}
