package proxy

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A recording of the set's background answers at most one call in a
// replay's test windows: one of the worker scope's (mappings.yaml worker:),
// and, under the background rules, one of the pool's (a recording no test
// lists). An application that makes such a call in the background (a timer,
// a janitor's sweep) makes it as often as the replay's timing lets it, so a
// later identical call can land in another test's window after the
// recording answered one. That call misses, and its miss names the
// recording, its scope and when it answered (models.RepeatNote.Background):
// judging reads that the call is the background's, not the test's. Before,
// a background spend was never noted as an answer, so the miss named
// nothing ("no_mocks, candidates 0"), and the test whose window the call
// landed in failed on a call it never made at record.
func TestABackgroundRecordingsLaterIdenticalMissNamesIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		name  string
		scope string
		serve func(mm *MockManager, names []string)
	}{
		{"worker scope", models.RepeatBackgroundWorker, func(mm *MockManager, names []string) {
			mm.ServeExactWorker(names)
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}},
		{"pool under the background rules", models.RepeatBackgroundPool, func(mm *MockManager, names []string) {
			mm.ServeExactBackground(models.RecordedWithS12Routing, names, nil)
			mm.ServeExactPool(models.PoolKeyNone, names)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			bg := workerProbe("mock-bg-x", time.Second, "/x", "background")
			mm := stagedManager(t, nil, []*models.Mock{bg})
			names := []string{bg.Name}
			c.serve(mm, names)
			mm.ServeExactTest("test-a", nil)
			if !mm.ExactServing() {
				t.Fatal("precondition: test-a is not served exactly")
			}
			if got, miss := httpMissCall(t, mm, "/x"); got != "background" {
				t.Fatalf("test-a's GET /x was answered %q (miss %+v); want the background's recording", got, miss)
			}
			if c.scope == models.RepeatBackgroundPool {
				mm.ServeExactPool(models.PoolKeyNone, names)
			} else {
				mm.ServeExactPool(models.PoolKeyNone, nil)
			}
			mm.ServeExactTest("test-b", nil)
			got, miss := httpMissCall(t, mm, "/x")
			if got != "" {
				t.Fatalf("test-b's GET /x was answered %q by a background recording that already answered one call", got)
			}
			if miss.Repeat == nil || miss.Repeat.Mock != bg.Name || miss.ClosestMock != bg.Name {
				t.Fatalf("test-b's GET /x missed with closest %q and repeat note %+v; want both naming %s, the background recording that answered test-a's call", miss.ClosestMock, miss.Repeat, bg.Name)
			}
			if miss.Repeat.Background != c.scope {
				t.Fatalf("the note names %s's scope %q; want %q", bg.Name, miss.Repeat.Background, c.scope)
			}
			if miss.Repeat.AnsweredAt.IsZero() {
				t.Fatal("the note does not say when the background recording answered")
			}
			if !strings.Contains(miss.NextSteps, c.scope) || strings.Contains(miss.NextSteps, "a recording of an earlier test") {
				t.Fatalf("the miss's next step does not name the recording's scope %q, or calls it a test's: %s", c.scope, miss.NextSteps)
			}
		})
	}
}

// A test's own answered recording is no background: an identical later
// call of another test names it as before, with no scope. And a test that
// recorded the call itself and makes it once more than it recorded misses as
// that extra call, with no note, whatever background recording answered
// before: the call repeats the test's own, so it stays the test's.
func TestABackgroundNoteNeverNamesATestsOwnRecordingOrItsExtraCall(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	bg := workerProbe("mock-bg-x", time.Second, "/x", "background")
	ownA := workerProbe("mock-own-a", 2*time.Second, "/y", "own-a")
	ownB := workerProbe("mock-own-b", 4*time.Second, "/x", "own-b")
	mm := stagedManager(t, nil, []*models.Mock{bg, ownA, ownB})
	mm.ServeExactWorker([]string{bg.Name})
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, ownA.Name)})
	if got, miss := httpMissCall(t, mm, "/x"); got != "background" {
		t.Fatalf("test-a's GET /x was answered %q (miss %+v); want the worker scope's", got, miss)
	}
	if got, miss := httpMissCall(t, mm, "/y"); got != "own-a" {
		t.Fatalf("test-a's GET /y was answered %q (miss %+v); want its own", got, miss)
	}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-b", []*models.Mock{roundMock(mm, ownB.Name)})
	if got, miss := httpMissCall(t, mm, "/x"); got != "own-b" {
		t.Fatalf("test-b's GET /x was answered %q (miss %+v); want its own", got, miss)
	}
	got, miss := httpMissCall(t, mm, "/x")
	if got != "" {
		t.Fatalf("test-b's second GET /x, one more than it recorded, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 || miss.Repeat != nil {
		t.Fatalf("test-b's second GET /x missed as %q, recorded %d, note %+v; want %q, recorded 1, no note", miss.MissKind, miss.Recorded, miss.Repeat, models.MissExtraCall)
	}
	got, miss = httpMissCall(t, mm, "/y")
	if got != "" {
		t.Fatalf("test-b's GET /y, test-a's own call, was answered %q", got)
	}
	if miss.Repeat == nil || miss.Repeat.Mock != ownA.Name || miss.Repeat.Background != "" {
		t.Fatalf("test-b's GET /y has note %+v; want one naming test-a's own %s with no background scope", miss.Repeat, ownA.Name)
	}
}
