package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A call the lookup named no recording for (FindStagedNotOwned) is asked
// whether it is a recording an earlier miss of the staging was named as
// (FindNamedAgain): each is named for one miss at most, as it answers one
// call at most, so such a call is one more than the recordings made
// elsewhere hold. It names nothing until a miss was named, never a
// recording no miss was named as, the open test's own, one the call's read
// held as its test's own (though that test ended before the miss was
// decided), one a call consumed, nor one of a staging the call was not made
// in; identical calls name the first in recorded order. Nothing is claimed:
// asked twice, it names the same.
func TestFindNamedAgainNamesTheRecordingAnEarlierMissWasNamedAs(t *testing.T) {
	own := httpCall("own", 0, "/orders")
	other := httpCall("other", time.Second, "/delete")
	consumed := httpCall("consumed", 1700*time.Millisecond, "/notify")
	pollA := httpCall("poll-a", 5*time.Second, "/jobs")
	pollB := httpCall("poll-b", 6*time.Second, "/jobs")
	all := []*models.Mock{own, other, consumed, pollA, pollB}
	mm := stagedManager(t, nil, all)
	byName := func(name string) func(*models.Mock) bool {
		return func(m *models.Mock) bool { return m.Name == name }
	}
	jobs := func(m *models.Mock) bool { return m.Spec.HTTPReq.URL == "/jobs" }
	find := func(match func(*models.Mock) bool) string { return stagedNotOwnedNow(mm, models.HTTP, match) }
	again := func(match func(*models.Mock) bool) string {
		return mm.FindNamedAgain(models.HTTP, mm.MarkCallRound(), nil, match)
	}

	mm.ServeExactPool("k", nil)
	if got := again(byName("other")); got != "" {
		t.Fatalf("with no miss named, a recording was named again: %q", got)
	}
	if got := find(byName("other")); got != "other" {
		t.Fatalf("precondition: the first miss named %q, want other", got)
	}
	if got := find(byName("other")); got != "" {
		t.Fatalf("precondition: a second miss named %q", got)
	}
	if a, b := again(byName("other")), again(byName("other")); a != "other" || b != "other" {
		t.Errorf("a recording a miss was named as was named again %q, then %q; want other both times", a, b)
	}
	if got := (&scopedMockDb{MockMemDb: mm}).FindNamedAgain(models.HTTP, mm.MarkCallRound(), nil, byName("other")); got != "other" {
		t.Errorf("a scoped view does not forward the lookup: %q", got)
	}
	if got := again(byName("poll-a")); got != "" {
		t.Errorf("a recording no miss was named as was named again: %q", got)
	}
	if a, b, none, rep := find(jobs), find(jobs), find(jobs), again(jobs); a != "poll-a" || b != "poll-b" || none != "" || rep != "poll-a" {
		t.Errorf("three identical calls named %q, %q, %q, and the third again %q; want poll-a, poll-b, none, then poll-a", a, b, none, rep)
	}

	// Named before its test, then its test's own: never named again there.
	if got := find(byName("own")); got != "own" {
		t.Fatalf("precondition: own was not named before its test: %q", got)
	}
	mm.ServeExactTest("T1", []*models.Mock{own})
	if got := again(byName("own")); got != "" {
		t.Errorf("in T1 its own recording was named again: %q", got)
	}
	// A call T1's read held own for, decided after T1 ended: the view the
	// call is decided in does not hold own, but the call's read did, so it
	// is never named again there either.
	held := mm.MarkCallRound()
	mm.ServeExactPool("k", nil)
	if got := mm.FindNamedAgain(models.HTTP, held, nil, byName("own")); got != "" {
		t.Errorf("a call whose read held own as T1's own, decided after T1 ended, named it again: %q", got)
	}

	// Named, then consumed by its test: never named again after.
	mm.ServeExactPool("k", nil)
	if got := find(byName("consumed")); got != "consumed" {
		t.Fatalf("precondition: consumed was not named: %q", got)
	}
	mm.ServeExactTest("T2", []*models.Mock{consumed})
	if !mm.DeleteFilteredMock(*consumed) {
		t.Fatal("T2 could not consume its own recording")
	}
	mm.ServeExactPool("k", nil)
	if got := again(byName("consumed")); got != "" {
		t.Errorf("a recording its test consumed was named again: %q", got)
	}

	// A call made before the next staging names nothing; the next staging
	// starts with no miss named.
	call := mm.MarkCallRound()
	mm.SetMocksWithWindow(nil, all, models.BaseTime, time.Now())
	mm.ServeExactPool("k", nil)
	if got := mm.FindNamedAgain(models.HTTP, call, nil, byName("other")); got != "" {
		t.Errorf("a call of the previous staging named %q again", got)
	}
	if got := again(byName("other")); got != "" {
		t.Errorf("the next staging named %q again before any miss named it", got)
	}
}

// With keys, FindNamedAgain reads the recordings the agent keeps on disk, as
// the lookup that named them did (FindStagedNotOwnedByKeys); without, only
// the trees.
func TestFindNamedAgainByKeysReadsTheRecordingSource(t *testing.T) {
	pool := httpCall("pool", 0, "/pool")
	onDisk := httpCall("t2-delete", time.Second, "/delete")
	mm := stagedManager(t, nil, []*models.Mock{pool})
	mm.SetRecordingSource(func(kind models.Kind, keys []string, visit func(string, func() (*models.Mock, error)) bool) error {
		if kind == models.HTTP && len(keys) == 1 && keys[0] == "DELETE /delete" {
			visit(onDisk.Name, func() (*models.Mock, error) { return onDisk, nil })
		}
		return nil
	})
	mm.ServeExactPool("k", []string{"pool"})
	match := func(m *models.Mock) bool { return m.Name == "t2-delete" }
	keys := []string{"DELETE /delete"}
	if got := stagedNotOwnedByKeysNow(mm, models.HTTP, keys, match); got != "t2-delete" {
		t.Fatalf("precondition: the recording kept on disk was not named: %q", got)
	}
	if got := mm.FindNamedAgain(models.HTTP, mm.MarkCallRound(), nil, match); got != "" {
		t.Errorf("without keys the recording source was read, naming %q again", got)
	}
	if got := mm.FindNamedAgain(models.HTTP, mm.MarkCallRound(), keys, match); got != "t2-delete" {
		t.Errorf("with keys the recording kept on disk was not named again: %q", got)
	}
}
