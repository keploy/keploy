package manager

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Every request whose test case an ingress could not hand on is counted, over
// the manager's life, and warned of as it comes: the first, and then every
// sendDropSampleRate-th, so a stop that loses many does not flood the log. A
// nil manager counts nothing and does not panic.
func TestEveryTestCaseNotHandedOnIsCountedAndTheFirstIsSaid(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	m := New(zap.NewNop())
	m.SetLogger(zap.New(core))
	for i := uint64(0); i < sendDropSampleRate; i++ {
		m.NoteNotHandedOn()
	}
	if got := m.NotHandedOn(); got != sendDropSampleRate {
		t.Fatalf("counted %d, want %d", got, sendDropSampleRate)
	}
	said := logs.FilterMessage(notHandedOnWarning).All()
	if len(said) != 2 || said[0].ContextMap()["not_handed_on_so_far"] != uint64(1) ||
		said[1].ContextMap()["not_handed_on_so_far"] != sendDropSampleRate {
		t.Fatalf("warned %d times (%v), want at the first and the %d-th", len(said), said, sendDropSampleRate)
	}

	var none *SyncMockManager
	none.NoteNotHandedOn()
	if none.NotHandedOn() != 0 {
		t.Fatal("a nil manager counted")
	}
}
