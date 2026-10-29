package replay

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// cutRecordingConf is a test-set config that says its recording's stop had to
// kill the app (models.TestSet.RecordStopKilled).
type cutRecordingConf struct {
	prTestSetConf
	why string
}

func (c cutRecordingConf) Read(context.Context, string) (*models.TestSet, error) {
	return &models.TestSet{RecordStopKilled: c.why}, nil
}

const recordingKill = "keploy stopped the application at the recording's end and a process of it had not exited 3s later (the grace keploy's stop gives each of its process groups), so keploy killed it"

// P101-b-k: a recording whose stop had to kill the app lacks what the app did
// as it stopped. A call the replay's app makes after the set's last test that
// no mock answered may be one of those, so under the proven switch it leaves
// the set not judged, naming the recording's kill and the call, instead of
// failing it. Without such a call the set is judged as any other.
func TestRunTestSet_ACallAfterTheLastTestOfARecordingItsStopCutIsNotJudged(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.testSetConf = cutRecordingConf{why: recordingKill}
	var reads atomic.Int32
	h.instr.mockErrorsFor = counted(&reads, misses(nil, nil, afterLast))

	status := h.run(t)

	rep := h.report.report
	if status != models.TestSetStatusInternalErr || rep.Status != string(models.TestSetStatusInternalErr) {
		t.Fatalf("status %s (report %s); a call after the last test of a recording its stop cut must leave the set not judged", status, rep.Status)
	}
	if !h.replayer.notJudged("test-set-0") {
		t.Fatal("the set is not marked not judged")
	}
	reason := rep.FailureReason
	if !strings.Contains(reason, "the recording's stop had to kill the application") || !strings.Contains(reason, recordingKill) || !strings.Contains(reason, "POST /publish to broker:9099") {
		t.Fatalf("failure reason %q does not name the recording's kill and the call", reason)
	}
	if len(rep.UnclaimedCalls) != 1 {
		t.Fatalf("unclaimed calls %+v; want the publish still named", rep.UnclaimedCalls)
	}

	h = newPartialRunHarness(t, 2, 0)
	h.replayer.testSetConf = cutRecordingConf{why: recordingKill}
	h.instr.mockErrorsFor = misses(nil, nil, nil)
	if status := h.run(t); status != models.TestSetStatusPassed || h.replayer.notJudged("test-set-0") {
		t.Fatalf("no call after the last test: status %s, not judged %v; want PASSED, judged", status, h.replayer.notJudged("test-set-0"))
	}
}

// Item (b): a call the agent saw made after it read the last test's misses
// (marked Background) is background work, made as the application stopped:
// the set is judged and passes, the call listed with where it was made,
// whether or not the recording's stop had to kill the application. Only a
// call filed late in the last test's window leaves such a set not judged.
func TestRunTestSet_ACallMadeAfterTheLastReadOfARecordingItsStopCutIsBackground(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.testSetConf = cutRecordingConf{why: recordingKill}
	h.instr.mockErrorsFor = misses(nil, nil, afterLastBackground)

	if status := h.run(t); status != models.TestSetStatusPassed || h.replayer.notJudged("test-set-0") {
		t.Fatalf("status %s (reason %q), not judged %v; want PASSED, judged", status, h.report.report.FailureReason, h.replayer.notJudged("test-set-0"))
	}
	if got := h.report.report.BackgroundCalls; len(got) != 1 || got[0].BackgroundWhy != madeAfterLast {
		t.Fatalf("background calls %+v; want the publish, made after the last test", got)
	}
}
