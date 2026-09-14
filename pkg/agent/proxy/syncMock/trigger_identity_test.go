package manager

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The trigger identity exemption is an additive branch in ResolveRange keyed
// on a metadata tag NO parser in this tree emits. These tests pin both halves:
// that a tagged mock is bound by identity, and — more importantly — that an
// untagged one is still bound by timestamp, exactly as before.

func taggedMock(name string, at time.Time, owner string) *models.Mock {
	m := &models.Mock{
		Name: name,
		Kind: models.KAFKA,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: at,
			ResTimestampMock: at,
		},
	}
	if owner != "" {
		m.Spec.Metadata[models.MetaKeyUnitTest] = owner
	}
	return m
}

func drain[T any](ch chan T) []T {
	var out []T
	for {
		select {
		case v := <-ch:
			out = append(out, v)
		default:
			return out
		}
	}
}

// A tagged mock goes to the test it names, even though its timestamp falls
// inside a DIFFERENT window. That is the whole point: a consumer unit's window
// opens at its trigger's response time, so the trigger's own request time sits
// inside the previous unit's window.
func TestResolveRangeBindsATaggedTriggerToItsOwnTest(t *testing.T) {
	m := New(nil)
	out := make(chan *models.Mock, 16)
	mappings := make(chan models.TestMockMapping, 16)
	m.SetOutputChannel(out)
	m.SetMappingChannel(context.Background(), mappings)
	m.SetFirstRequestSignaled()
	m.resolvedTestCount = models.StartupMockTestCaseWindow + 1

	base := time.Now()
	// This mock's timestamp is squarely inside test-1's window, but it is
	// tagged as test-2's trigger.
	m.AddMock(taggedMock("trigger-for-test-2", base.Add(50*time.Millisecond), "test-2"))
	m.AddMock(taggedMock("plain-dependency", base.Add(60*time.Millisecond), ""))

	m.ResolveRange(base, base.Add(100*time.Millisecond), "test-1", true, true)
	m.ResolveRange(base.Add(100*time.Millisecond), base.Add(200*time.Millisecond), "test-2", true, true)

	byTest := map[string]int{}
	for _, e := range drain(mappings) {
		byTest[e.TestName] += len(e.MockIDs)
	}
	if byTest["test-1"] != 1 {
		t.Fatalf("test-1 mapped %d mocks, want only its own in-window dependency: %v", byTest["test-1"], byTest)
	}
	if byTest["test-2"] != 1 {
		t.Fatalf("test-2 mapped %d mocks, want its tagged trigger: %v", byTest["test-2"], byTest)
	}
	for _, mk := range drain(out) {
		if _, leaked := mk.Spec.Metadata[models.MetaKeyUnitTest]; leaked {
			t.Fatalf("the in-flight unit tag reached persistence on %q", mk.Name)
		}
	}
}

// THE NO-EXEMPTION PATH IS UNCHANGED. Every mock any parser in this tree emits
// is untagged, so this is the behaviour of every existing recording: bin by
// timestamp, one mapping per window, nothing else touched.
func TestResolveRangeIsUnchangedForUntaggedMocks(t *testing.T) {
	m := New(nil)
	out := make(chan *models.Mock, 16)
	mappings := make(chan models.TestMockMapping, 16)
	m.SetOutputChannel(out)
	m.SetMappingChannel(context.Background(), mappings)
	m.SetFirstRequestSignaled()
	// Past the startup window, so the startup rescue (which flushes an
	// out-of-window mock without a mapping entry) is not what is being
	// measured here.
	m.resolvedTestCount = models.StartupMockTestCaseWindow + 1

	base := time.Now()
	m.AddMock(taggedMock("in-window-1", base.Add(10*time.Millisecond), ""))
	m.AddMock(taggedMock("in-window-2", base.Add(20*time.Millisecond), ""))
	m.AddMock(taggedMock("next-window", base.Add(150*time.Millisecond), ""))

	m.ResolveRange(base, base.Add(100*time.Millisecond), "test-1", true, true)
	m.ResolveRange(base.Add(100*time.Millisecond), base.Add(200*time.Millisecond), "test-2", true, true)

	byTest := map[string]int{}
	for _, e := range drain(mappings) {
		byTest[e.TestName] += len(e.MockIDs)
	}
	if byTest["test-1"] != 2 || byTest["test-2"] != 1 {
		t.Fatalf("untagged binning changed: %v (want test-1:2 test-2:1)", byTest)
	}
	if got := len(drain(out)); got != 3 {
		t.Fatalf("%d mocks reached persistence, want 3", got)
	}
}

// A tagged mock whose unit has not resolved yet is RETAINED, not reaped by the
// stale-buffer cutoff — a consumer that idles between messages routinely
// leaves a trigger sitting for longer than the seven-second horizon.
func TestATaggedTriggerSurvivesTheStaleCutoffUntilItsUnitResolves(t *testing.T) {
	m := New(nil)
	out := make(chan *models.Mock, 16)
	mappings := make(chan models.TestMockMapping, 16)
	m.SetOutputChannel(out)
	m.SetMappingChannel(context.Background(), mappings)
	m.SetFirstRequestSignaled()
	// Past the startup window, so the startup rescue cannot be what saves
	// it — this must be the exemption or nothing.
	m.resolvedTestCount = models.StartupMockTestCaseWindow + 1

	old := time.Now().Add(-30 * time.Second)
	m.AddMock(taggedMock("trigger-for-test-9", old, "test-9"))

	// Several unrelated windows resolve first.
	for i := 0; i < 3; i++ {
		m.ResolveRange(time.Now(), time.Now().Add(time.Millisecond), "test-"+string(rune('1'+i)), true, true)
	}
	if got := len(drain(out)); got != 0 {
		t.Fatalf("the tagged trigger was flushed to the wrong test (%d mocks out)", got)
	}

	m.ResolveRange(time.Now(), time.Now().Add(time.Millisecond), "test-9", true, true)
	entries := drain(mappings)
	var mapped int
	for _, e := range entries {
		if e.TestName == "test-9" {
			mapped += len(e.MockIDs)
		}
	}
	if mapped != 1 {
		t.Fatalf("test-9 mapped %d mocks, want its 30-second-old trigger: %v", mapped, entries)
	}
}
