package proxy

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// repeatRun stages one test's recordings, recorded in its window, and opens
// the test under exact serving with the ones repeat names marked
// `repeat: any` (MockEntry.Repeat, carried on the copy the agent serves as
// TestModeInfo.Repeat).
func repeatRun(t *testing.T, recs []*models.Mock, repeat ...string) (*Proxy, *MockManager) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	start := recs[0].Spec.ReqTimestampMock.Add(-time.Millisecond)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow(recs, nil, models.BaseTime, time.Now().Add(time.Hour))
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	owned := make([]*models.Mock, 0, len(recs))
	for _, r := range recs {
		c := roundMock(mm, r.Name).ShallowCopy()
		c.TestModeInfo.Repeat = slices.Contains(repeat, r.Name)
		owned = append(owned, c)
	}
	mm.ServeExactTest("test-0", owned)
	p.ContinueTestErrorCapture()
	return p, mm
}

// repeatOutcome is what the test's end reads: the recordings that answered,
// those that did not, and the calls no recording answered.
func repeatOutcome(t *testing.T, p *Proxy, mm *MockManager) (consumed, unconsumed []string, misses int) {
	t.Helper()
	_, consumed, unconsumed, ok := mm.ExactTestOutcome()
	if !ok {
		t.Fatal("exact serving is off")
	}
	m, _ := p.GetMockErrors(context.Background())
	return consumed, unconsumed, len(m)
}

// A recording the test's mappings entry marks `repeat: any` answers the
// test's identical read once or twice or three times, and every replay
// passes: the read the page makes once or twice by its timing (a GET a
// second effect asks for before the first answer lands) is the test's own
// at any count. It is still required: a replay that never makes the read
// leaves it unconsumed, and the test fails.
func TestARecordingMarkedRepeatAnyAnswersItsTestsIdenticalReadAtAnyCount(t *testing.T) {
	at := time.Now().Add(time.Second)
	for _, reads := range []int{0, 1, 2, 3} {
		p, mm := repeatRun(t, []*models.Mock{r3Mock("mock-cluster", at, "GET", "/cluster?name=c1", "", `{"name":"c1"}`)}, "mock-cluster")
		h := httpint.New(zap.NewNop())
		for j := 0; j < reads; j++ {
			code, body := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet("/cluster?name=c1"))
			if code != http.StatusOK || body != `{"name":"c1"}` {
				t.Fatalf("%d reads: read %d got %d %q; want the marked recording's answer", reads, j+1, code, body)
			}
		}
		consumed, unconsumed, misses := repeatOutcome(t, p, mm)
		if reads == 0 {
			if !slices.Equal(unconsumed, []string{"mock-cluster"}) {
				t.Fatalf("no read: unconsumed %v; want the marked recording, still required of its test", unconsumed)
			}
			continue
		}
		if misses != 0 || len(unconsumed) != 0 || !slices.Equal(consumed, []string{"mock-cluster"}) {
			t.Fatalf("%d reads: consumed %v, unconsumed %v, misses %d; want the marked recording answered, nothing left, no miss", reads, consumed, unconsumed, misses)
		}
	}
}

// Without the mark the old exact rule holds: a test's recording answers one
// identical read, and a second is no answer of its (keploy/unmocked_call).
func TestAnUnmarkedRecordingAnswersItsTestsIdenticalReadOnce(t *testing.T) {
	at := time.Now().Add(time.Second)
	p, mm := repeatRun(t, []*models.Mock{r3Mock("mock-cluster", at, "GET", "/cluster?name=c1", "", `{"name":"c1"}`)})
	h := httpint.New(zap.NewNop())
	if code, body := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet("/cluster?name=c1")); code != http.StatusOK || body != `{"name":"c1"}` {
		t.Fatalf("first read got %d %q", code, body)
	}
	code, body := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet("/cluster?name=c1"))
	if _, _, misses := repeatOutcome(t, p, mm); code == http.StatusOK && body == `{"name":"c1"}` || misses == 0 {
		t.Fatalf("an unmarked recording answered a second identical read (%d %q, %d misses); want the second read refused", code, body, misses)
	}
}

// The mark answers only an identical read: a read the recording does not
// match (another query) is no answer of its, and misses.
func TestAMarkedRecordingNeverAnswersAReadItDoesNotMatch(t *testing.T) {
	at := time.Now().Add(time.Second)
	p, mm := repeatRun(t, []*models.Mock{r3Mock("mock-cluster", at, "GET", "/cluster?name=c1", "", `{"name":"c1"}`)}, "mock-cluster")
	h := httpint.New(zap.NewNop())
	r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet("/cluster?name=c1"))
	code, body := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet("/cluster?name=c2"))
	if _, _, misses := repeatOutcome(t, p, mm); body == `{"name":"c1"}` || misses == 0 {
		t.Fatalf("a read of another cluster got %d %q with %d misses; want a miss", code, body, misses)
	}
}

// A recording the test made of the same read with a different answer stays
// required beside a marked one, and the mark never passes a replay that
// skipped it: a replay that reads once leaves the other unconsumed, and one
// that reads twice is answered the marked recording's answer again (its
// read is identical), so the other is left unconsumed too, and the test
// fails. That is why keploy proposes the mark only for a read whose every
// recording is equal, request and answer.
func TestAMarkedRecordingNeverHidesADifferentRecordedAnswer(t *testing.T) {
	at := time.Now().Add(time.Second)
	recs := func() []*models.Mock {
		return []*models.Mock{
			r3Mock("mock-a", at, "GET", "/cluster?name=c1", "", `{"state":"pending"}`),
			r3Mock("mock-b", at.Add(time.Millisecond*5), "GET", "/cluster?name=c1", "", `{"state":"ready"}`),
		}
	}
	for _, reads := range []int{1, 2, 3} {
		p, mm := repeatRun(t, recs(), "mock-a")
		h := httpint.New(zap.NewNop())
		var got []string
		for range reads {
			_, body := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet("/cluster?name=c1"))
			got = append(got, body)
		}
		if _, unconsumed, _ := repeatOutcome(t, p, mm); !slices.Equal(unconsumed, []string{"mock-b"}) {
			t.Fatalf("%d reads answered %q, unconsumed %v; want mock-b, the other answer, left unconsumed: the test fails", reads, got, unconsumed)
		}
	}
}

// A write its test's mappings entry marks `repeat: any` is served as the
// mark says: keploy proposes the mark only for a read, but a person who
// writes it on a write owns that choice (it shows in the lock's diff), so
// the recorded answer serves each identical write at any count, once at
// least, as for a read.
func TestAMarkedWriteIsHonouredAsWritten(t *testing.T) {
	at := time.Now().Add(time.Second)
	const body = `{"name":"c1"}`
	for _, writes := range []int{1, 2} {
		p, mm := repeatRun(t, []*models.Mock{r3Mock("mock-save", at, "POST", "/cluster", body, `{"saved":true}`)}, "mock-save")
		h := httpint.New(zap.NewNop())
		for j := 0; j < writes; j++ {
			if code, got := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, pooledPost("/cluster", body)); code != http.StatusOK || got != `{"saved":true}` {
				t.Fatalf("%d writes: write %d got %d %q; want the marked recording's answer", writes, j+1, code, got)
			}
		}
		if consumed, unconsumed, misses := repeatOutcome(t, p, mm); misses != 0 || len(unconsumed) != 0 || !slices.Equal(consumed, []string{"mock-save"}) {
			t.Fatalf("%d writes: consumed %v, unconsumed %v, misses %d; want the marked write answered, nothing left, no miss", writes, consumed, unconsumed, misses)
		}
	}
}
