package mock

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A record's full round through its real doors: a MockManager serves o::A's
// own mock-1 in o::A's window, then o::A's mock-3, o::B's mock-2 and the
// session's session-0 in o::B's window, and o::A never consumes mock-3
// itself. reportOutcome reads what answered where and learns, and
// writeTestsReceipt writes the receipt. Read back from the file, o::A's line
// lists mock-3 under learnedAnsweredElsewhere, its owner's line, and the
// set's learned holds the session's recording alone: what the round learned
// onto mock-3 is said once, where its owner reads it, never dropped and never
// with the set's.
func TestTheReceiptListsWhatWasLearnedOntoATestsRecordingOnlyAnotherTestConsumedOnItsOwnersLine(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	serve := func(name string, lt models.Lifetime) {
		t.Helper()
		require.True(t, mm.MarkMockAsUsed(models.Mock{Name: name, Kind: models.HTTP, TestModeInfo: models.TestModeInfo{Lifetime: lt}}), name)
	}
	tick := func() time.Time {
		time.Sleep(3 * time.Millisecond)
		now := time.Now()
		time.Sleep(3 * time.Millisecond)
		return now
	}
	aStart := tick()
	mm.OpenTestWindow(1)
	serve("mock-1", models.LifetimePerTest)
	mm.CloseTestWindow(1)
	aEnd := tick()
	bStart := tick()
	mm.OpenTestWindow(2)
	serve("mock-3", models.LifetimePerTest)
	serve("mock-2", models.LifetimePerTest)
	serve("session-0", models.LifetimeSession)
	mm.CloseTestWindow(2)
	bEnd := tick()
	windows := []models.ScopeWindow{
		{Name: "o::A", Seq: 1, Start: aStart, End: aEnd, Outcome: models.ScopeOutcomePassed},
		{Name: "o::B", Seq: 2, Start: bStart, End: bEnd, Outcome: models.ScopeOutcomePassed},
	}
	expected := map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}, {Name: "mock-3"}}, "o::B": {{Name: "mock-2"}}}
	dir := t.TempDir()
	cfg := &config.Config{Path: dir}
	cfg.Test.Learn = true
	m := &mockService{config: cfg, mockDB: &learnedStore{}, logger: zap.NewNop(),
		instrumentation: outcomeInstr{clientTestsInstr: clientTestsInstr{reads: true}, consumed: mm.GetConsumedMocks()}}
	counts := m.reportOutcome(context.Background(), 4, replayDetail{set: "s", windows: windows, expected: expected}, func() bool { return false })
	require.NoError(t, counts.learnErr)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "s"), 0o755))
	writeTestsReceipt(zap.NewNop(), dir, TestsReceipt{Set: "s", Tests: counts.tests, Answered: counts.answered, Learned: counts.learnedPool, LearnedElsewhere: counts.learnedElsewhere})
	raw, err := os.ReadFile(filepath.Join(dir, "s", TestsReceiptFile))
	require.NoError(t, err)
	if !strings.Contains(string(raw), "learnedAnsweredElsewhere:") {
		t.Fatalf("the receipt has no learnedAnsweredElsewhere key:\n%s", raw)
	}
	got, err := ReadTestsReceipt(dir, "s")
	require.NoError(t, err)
	lines := map[string]TestReceipt{}
	for _, tr := range got.Tests {
		lines[tr.Name] = tr
	}
	if a := lines["o::A"]; !reflect.DeepEqual(a.Learned, []string{"mock-1 body.ts (noise)"}) || !reflect.DeepEqual(a.LearnedAnsweredElsewhere, []string{"mock-3 body.ts (noise)"}) {
		t.Fatalf("o::A's line learned %v, answered elsewhere %v; want its mock-1 as its own and its mock-3 marked as answered elsewhere\n%s", a.Learned, a.LearnedAnsweredElsewhere, raw)
	}
	if b := lines["o::B"]; !reflect.DeepEqual(b.Learned, []string{"mock-2 body.ts (noise)"}) || len(b.LearnedAnsweredElsewhere) != 0 {
		t.Fatalf("o::B's line learned %v, answered elsewhere %v; want its own mock-2 alone\n%s", b.Learned, b.LearnedAnsweredElsewhere, raw)
	}
	if !reflect.DeepEqual(got.Learned, []string{"session-0 body.ts (noise)"}) || slices.ContainsFunc(got.Learned, func(f string) bool { return strings.HasPrefix(f, "mock-3 ") }) {
		t.Fatalf("the set's learned %v; want the session's recording alone, never o::A's mock-3\n%s", got.Learned, raw)
	}
}
