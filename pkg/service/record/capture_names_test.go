package record

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	capA = models.CaptureIDPrefix + "n0nce-1"
	capB = models.CaptureIDPrefix + "n0nce-2"
	capC = models.CaptureIDPrefix + "n0nce-3"
)

// What a capture id's test case was saved under, and when that is decided.
func TestCaptureNames_NameOf(t *testing.T) {
	c := newCaptureNames()
	decidedAs := func(id, want string) {
		t.Helper()
		got, decided := c.nameOf(id)
		require.True(t, decided, "%s must be decided", id)
		require.Equal(t, want, got, "the name %s is filed under", id)
	}
	undecided := func(id string) {
		t.Helper()
		_, decided := c.nameOf(id)
		require.False(t, decided, "%s's test case may still come: it must not be decided", id)
	}

	// A name that is no capture id is its own, from the start.
	decidedAs("test-7", "test-7")
	decidedAs("get-kind-alpha-1", "get-kind-alpha-1")
	decidedAs("", "")

	undecided(capA)
	c.inserted(capA, "get-kind-alpha-1", nil)
	decidedAs(capA, "get-kind-alpha-1")

	c.inserted(capB, "get-kind-beta-1", errors.New("no space left on device"))
	decidedAs(capB, "") // not saved: what names it is left out

	c.inserted(capC, "", nil) // taken, and nothing written (no name)
	decidedAs(capC, "")

	// A test case with a name of its own binds nothing.
	c.inserted("", "test-9", nil)
	decidedAs("", "")

	const late = models.CaptureIDPrefix + "n0nce-4"
	undecided(late)
	c.end()
	decidedAs(late, "") // the stream ended without it
	decidedAs(capA, "get-kind-alpha-1")
	c.end() // idempotent

	// No names: every name is filed as it comes.
	var none *captureNames
	n, decided := none.nameOf(capA)
	assert.True(t, decided)
	assert.Equal(t, capA, n)
	none.inserted(capA, "x", nil)
	none.end()
	assert.Nil(t, none.wakeCh())
}

// Every change wakes the consumer, and a burst of them never blocks the
// test-case loop.
func TestCaptureNames_WakesOnEveryChange(t *testing.T) {
	c := newCaptureNames()
	for i := 0; i < 3; i++ {
		c.inserted(capA, "a", nil) // a full wake channel must not block
	}
	select {
	case <-c.wakeCh():
	default:
		t.Fatal("a bind did not wake the consumer")
	}
	c.end()
	select {
	case <-c.wakeCh():
	default:
		t.Fatal("the stream's end did not wake the consumer")
	}
}

// mappedNow drains what the consumer has told so far, without waiting.
func mappedNow(c *consumerRun) []string {
	var got []string
	for {
		select {
		case v := <-c.mapped:
			got = append(got, v)
		default:
			return got
		}
	}
}

// A mapping that names a capture id before its test case is saved is held,
// and filed under the name the test case is saved with once it is: never
// under the capture id, never under "".
func TestConsumeMappings_CaptureIDHeldUntilItsTestCaseIsSaved(t *testing.T) {
	c := startConsumer(t)
	c.save("m1")
	c.save("m2")
	c.send(t, models.TestMockMapping{TestName: capA, MockIDs: []string{"m1"}})
	// A plain name after it: once it is told, the capture's entry was taken.
	c.send(t, models.TestMockMapping{TestName: "test-2", MockIDs: []string{"m2"}})
	assert.Equal(t, "test-2/m2", recvName(t, c.mapped, "test-2's mock"))
	assert.Empty(t, mappedNow(c), "the capture's entry was filed before its test case was saved")

	c.names.inserted(capA, "get-kind-alpha-1", nil)
	assert.Equal(t, "get-kind-alpha-1/m1", recvName(t, c.mapped, "the capture's mock under its test case's name"))

	// A later part of its mapping (a late-binned mock) goes straight there.
	c.save("m3")
	c.send(t, models.TestMockMapping{TestName: capA, MockIDs: []string{"m3"}})
	assert.Equal(t, "get-kind-alpha-1/m3", recvName(t, c.mapped, "the capture's late mock"))

	c.closeMappings()
	c.waits.end()
	c.finish(t)
	assert.Equal(t, []string{"m1", "m3"}, persistedFor(c.db, "get-kind-alpha-1"))
	assert.Empty(t, persistedFor(c.db, capA), "an entry was written under the capture id")
	assert.Empty(t, persistedFor(c.db, ""), "an entry was written under no name")
}

// A mapping with no name (an agent that gives no capture ids) is filed as it
// always was, under no name: only a capture id is translated.
func TestConsumeMappings_NoNameIsFiledAsBefore(t *testing.T) {
	c := startConsumer(t)
	c.save("m1")
	c.send(t, models.TestMockMapping{TestName: "", MockIDs: []string{"m1"}})
	assert.Equal(t, "/m1", recvName(t, c.mapped, "the nameless mapping's mock"))
	c.closeMappings()
	c.waits.end()
	c.finish(t)
	assert.Equal(t, []string{"m1"}, persistedFor(c.db, ""))
}

// A capture whose test case is not saved has its entries left out: its
// mocks stay saved, in no test's mapping.
func TestConsumeMappings_CaptureIDOfAnUnsavedTestCaseIsLeftOut(t *testing.T) {
	c := startConsumer(t)
	c.save("m1")
	c.send(t, models.TestMockMapping{TestName: capA, MockIDs: []string{"m1"}})
	c.names.inserted(capA, "", errors.New("no space left on device"))
	// A later part of its mapping (a late-binned mock), after its test case
	// is known not to be saved, is left out too.
	c.save("m3")
	c.send(t, models.TestMockMapping{TestName: capA, MockIDs: []string{"m3"}})
	c.save("m2")
	c.send(t, models.TestMockMapping{TestName: "test-2", MockIDs: []string{"m2"}})
	assert.Equal(t, "test-2/m2", recvName(t, c.mapped, "test-2's mock"))
	assert.Empty(t, mappedNow(c))

	c.closeMappings()
	c.waits.end()
	c.finish(t)
	assert.Empty(t, persistedFor(c.db, capA))
	assert.Empty(t, persistedFor(c.db, ""))
}

// A capture whose test case never comes keeps its entry held until the
// test-case stream ends, and the consumer does not return before: the test
// case may yet be saved.
func TestConsumeMappings_CaptureIDHeldUntilTheTestCaseStreamEnds(t *testing.T) {
	c := startConsumer(t)
	c.save("m1")
	c.send(t, models.TestMockMapping{TestName: capA, MockIDs: []string{"m1"}})
	c.closeMappings()
	c.waits.end()
	select {
	case <-c.done:
		t.Fatal("the consumer returned while a capture's test case could still be saved")
	case <-time.After(200 * time.Millisecond):
	}
	c.names.end()
	c.finish(t)
	assert.Empty(t, mappedNow(c))
	assert.Empty(t, persistedFor(c.db, capA))
}

// A capture's lost mock revokes its test case under the name it was saved
// with, which is the one the recording's revoke deletes.
func TestConsumeMappings_CaptureIDLostMockRevokesItsSavedName(t *testing.T) {
	c := startConsumer(t)
	c.drop("d1")
	c.send(t, models.TestMockMapping{TestName: capA, MockIDs: []string{"d1"}})
	c.names.inserted(capA, "get-kind-alpha-1", nil)
	assert.Equal(t, "get-kind-alpha-1", recvName(t, c.revoked, "the revoke under the saved name"))
	c.closeMappings()
	c.waits.end()
	c.finish(t)
}

// A revoke that names a capture id deletes the test case saved under it,
// and nothing for one that was not saved or never came.
func TestRevokedToDelete_CaptureIDs(t *testing.T) {
	names := newCaptureNames()
	names.inserted(capA, "get-kind-alpha-1", nil)
	names.inserted(capB, "", errors.New("refused"))
	inserted := map[string]struct{}{"get-kind-alpha-1": {}, "test-1": {}}
	got := revokedToDelete([]string{capA, capB, capC, "test-1"}, inserted, nil, names)
	slices.Sort(got)
	assert.Equal(t, []string{"get-kind-alpha-1", "test-1"}, got)
}

// The recording files a capture's mapping under the name its store gives the
// test case, whichever of the two comes first, and leaves out the mapping of
// a capture whose test case never came. A revoke naming the capture id
// deletes the test case it names.
func TestStart_CaptureIDMappingFiledUnderTheStoredName(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{}}, BaseRecordHooks{}, zap.New(core), func(r *Recorder) {
		r.testDB.(*recTestDB).autoName = "get-kind"
	})
	at := time.Now().Add(-time.Minute)
	tc := func(id string, req time.Time) *models.TestCase {
		return &models.TestCase{Kind: models.HTTP, CaptureID: id,
			HTTPReq: models.HTTPReq{Timestamp: req}, HTTPResp: models.HTTPResp{Timestamp: req.Add(time.Millisecond)}}
	}
	run.mock(t, "m1")
	run.mock(t, "m2")
	run.mock(t, "m3")
	// capA: its mapping comes before its test case.
	run.mapping(t, capA, "m1")
	select {
	case run.f.incoming <- tc(capA, at):
	case <-time.After(hangGuard):
		t.Fatal("recorder never consumed capA's test case")
	}
	waitForPersistedTestCase(t, run.testDB, "get-kind-1")
	// capB: its test case comes first.
	select {
	case run.f.incoming <- tc(capB, at.Add(time.Second)):
	case <-time.After(hangGuard):
		t.Fatal("recorder never consumed capB's test case")
	}
	waitForPersistedTestCase(t, run.testDB, "get-kind-2")
	run.mapping(t, capB, "m2")
	// capC: its test case never comes.
	run.mapping(t, capC, "m3")
	run.frame(t, revokeFrame("revoke-frame", capB))
	run.stop(t)

	assert.Equal(t, []string{"m1"}, persistedFor(run.mapDB, "get-kind-1"))
	assert.Equal(t, []string{"m2"}, persistedFor(run.mapDB, "get-kind-2"))
	for _, n := range []string{capA, capB, capC, ""} {
		assert.Empty(t, persistedFor(run.mapDB, n), "an entry was written under %q", n)
	}
	assert.Equal(t, []string{"get-kind-2"}, run.deleted(), "the revoke names capB: the test case saved for it")
	// The consumer returned once the test-case stream was over: capC's held
	// entry was dropped then, not given up with the consumer by the drain.
	assert.Zero(t, logs.FilterMessageSnippet("made no progress").Len(),
		"the recording's drain gave up a consumer still holding a capture's entry: %v", logs.All())
}
