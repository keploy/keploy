package manager

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// copyKind is a kind whose replay serves a mock by copy of its "copy"
// metadata, and needs no mock for one whose "copy" is "none"; a mock with
// no "copy", or one of "needed:" and a key, it needs in itself.
const copyKind models.Kind = "CopyServed"

// servesByCopy registers copyKind's ServedByCopy until the test ends.
func servesByCopy(t *testing.T) {
	t.Helper()
	RegisterServedByCopy(copyKind, func(mock *models.Mock) (string, bool) {
		switch k := mock.Spec.Metadata["copy"]; {
		case k == "":
			return "", false
		case k == "none":
			return "", true
		case strings.HasPrefix(k, "needed:"):
			return strings.TrimPrefix(k, "needed:"), false
		default:
			return k, true
		}
	})
	t.Cleanup(func() { RegisterServedByCopy(copyKind, nil) })
}

// copyAt is a session's mock of copyKind whose replay serves it from any
// copy of key, requested at at.
func copyAt(t *testing.T, key string, at time.Time) *models.Mock {
	t.Helper()
	m := sharedAt(t, "c1", at)
	m.Kind = copyKind
	m.Spec.Metadata["copy"] = key
	return m
}

// fullLaneAfter is fullLane on a channel whose consumer took each of taken
// first, as sent with room in the channel.
func fullLaneAfter(t *testing.T, m *SyncMockManager, taken ...*models.Mock) (chan *models.Mock, *stall.ManualClock) {
	t.Helper()
	ch := make(chan *models.Mock, len(taken)+1)
	clk := &stall.ManualClock{}
	m.outClock, m.outStall = clk, time.Hour
	m.SetOutputChannel(ch)
	for _, mock := range taken {
		if got := sendNow(m, mock, ""); got != MockTaken {
			t.Fatalf("setup: a mock sent with room in the channel: %q", got)
		}
	}
	ch <- &models.Mock{Name: "filler"}
	m.send(inHandMock(), "", nil)
	waitClockPending(t, clk)
	return ch, clk
}

// A session's mock lost on its way out, whose kind's replay serves it from
// any copy of its key, leaves out no test case once the recording's consumer
// took a copy requested no later than it: no test needs that mock. Nor does
// one its replay answers with no mock. It finds no room kept for the mocks a
// test may need either: it is bounded as a test's own. A mock of a key the
// consumer took none of, or took only a later-requested copy of, is still one
// any test may need: it finds the room, and its loss leaves out the test
// cases after it, at ERROR.
func TestALostMockServedFromACopyTheRecordingHoldsLeavesOutNone(t *testing.T) {
	servesByCopy(t)
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	t0 := time.Now().Add(-time.Minute)
	ch, _ := fullLaneAfter(t, m, copyAt(t, "k", t0))
	queueAt(t, outQueueMaxBytes-outQueueSharedRoom)
	if got := sendNow(m, copyAt(t, "k", t0.Add(time.Second)), ""); got != MockDroppedOutputFull {
		t.Fatalf("a mock whose copy the recording holds, in the room kept for the shared ones: %q, want it bounded as a test's (%q)", got, MockDroppedOutputFull)
	}
	if got := sendNow(m, copyAt(t, "k", t0), ""); got != MockDroppedOutputFull {
		t.Fatalf("a mock requested with its copy, in the room kept for the shared ones: %q, want it bounded as a test's (%q)", got, MockDroppedOutputFull)
	}
	if got := sendNow(m, copyAt(t, "none", t0), ""); got != MockDroppedOutputFull {
		t.Fatalf("a mock its replay needs none for, in the room kept for the shared ones: %q, want it bounded as a test's (%q)", got, MockDroppedOutputFull)
	}
	if leftOutAfter(m, t0) || leftOutLines(logs) != 0 {
		t.Fatal("a lost mock served from a copy the recording holds, or needing none, left out the tests after it")
	}
	if got := sendNow(m, copyAt(t, "k", t0.Add(-time.Second)), ""); got != MockTaken {
		t.Fatalf("a mock requested before the copy the recording holds: %q, want it in the room kept for it", got)
	}
	if got := sendNow(m, copyAt(t, "other", t0), ""); got != MockTaken {
		t.Fatalf("a mock of a key the recording holds no copy of: %q, want it in the room kept for it", got)
	}
	queueAt(t, outQueueSharedRoom)
	if got := sendNow(m, copyAt(t, "other", t0.Add(2*time.Second)), ""); got != MockDroppedOutputFull {
		t.Fatalf("a mock at the bound itself: %q, want %q", got, MockDroppedOutputFull)
	}
	if !leftOutAfter(m, t0.Add(2*time.Second)) || leftOutLines(logs) != 1 {
		t.Fatalf("a lost mock of a key the recording holds no copy of: left out %v, %d ERRORs; want the tests after it left out, said once", leftOutAfter(m, t0.Add(2*time.Second)), leftOutLines(logs))
	}
	sendNow(m, copyAt(t, "k", t0.Add(-time.Second)), "")
	if !leftOutAfter(m, t0.Add(-time.Second)) {
		t.Fatal("a lost mock requested before the copy the recording holds left out none of the tests after it")
	}
	for i := 0; i < 5; i++ { // the copy, the filler, the mock in hand, the two queued
		<-ch
	}
	waitOutQueue(t, m)
}

// A copy counts once the recording's consumer took it, through the queue as
// at once, and only for the recording whose channel took it. One queued and
// not yet taken, one taken by the last recording's consumer, an answer the
// client never read and one with no request time are no copy the recording
// holds: a mock of their key lost leaves out the test cases after it.
func TestACopyCountsOnceTheRecordingsConsumerTookIt(t *testing.T) {
	servesByCopy(t)
	t0 := time.Now().Add(-time.Minute)

	t.Run("queued", func(t *testing.T) {
		m := New(nil)
		ch, _ := fullLane(t, m)
		queueAt(t, outQueueMaxBytes-outQueueSharedRoom)
		if got := sendNow(m, copyAt(t, "k", t0), ""); got != MockTaken {
			t.Fatalf("setup: a copy in the room kept for it: %q", got)
		}
		queueAt(t, outQueueSharedRoom)
		if sendNow(m, copyAt(t, "k", t0.Add(time.Second)), ""); !leftOutAfter(m, t0.Add(time.Second)) {
			t.Fatal("a mock whose copy is only queued, lost, left out none of the tests after it")
		}
		for i := 0; i < 3; i++ { // the filler, the mock in hand, the queued copy
			<-ch
		}
		waitOutQueue(t, m)
	})

	t.Run("taken through the queue, then the next recording", func(t *testing.T) {
		m := New(nil)
		ch, clk := fullLane(t, m)
		sendNow(m, copyAt(t, "k", t0), "")
		for i := 0; i < 3; i++ { // the filler, the mock in hand, the copy
			<-ch
		}
		waitOutQueue(t, m)
		ch <- &models.Mock{Name: "filler"}
		m.send(inHandMock(), "", nil)
		waitClockPending(t, clk)
		queueAt(t, outQueueMaxBytes)
		if sendNow(m, copyAt(t, "k", t0.Add(time.Second)), ""); leftOutAfter(m, t0.Add(time.Second)) {
			t.Fatal("a mock whose copy the consumer took through the queue, lost, left out the tests after it")
		}
		<-ch // the filler
		<-ch // the mock in hand
		waitOutQueue(t, m)
		fullLane(t, m) // the next recording, on its own channel
		if sendNow(m, copyAt(t, "k", t0.Add(time.Second)), ""); !leftOutAfter(m, t0.Add(time.Second)) {
			t.Fatal("a mock whose copy only the last recording's consumer took, lost, left out none of the tests after it")
		}
		// The third, whose consumer takes a copy: after the second's span,
		// which its start closed.
		t1 := time.Now()
		fullLaneAfter(t, m, copyAt(t, "k", t1))
		if sendNow(m, copyAt(t, "k", t1.Add(5*time.Second)), ""); leftOutAfter(m, t1.Add(5*time.Second)) {
			t.Fatal("a mock whose copy the third recording's consumer took, lost, left out the tests after it")
		}
	})

	for _, c := range []struct {
		name string
		copy func() *models.Mock
	}{
		{"an answer the client never read", func() *models.Mock {
			c := copyAt(t, "k", t0)
			c.Spec.Metadata[models.MetadataUndeliveredKey] = models.MetadataUndelivered
			return c
		}},
		{"one with no request time", func() *models.Mock {
			c := copyAt(t, "k", t0)
			c.Spec.ReqTimestampMock = time.Time{}
			return c
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := New(nil)
			fullLaneAfter(t, m, c.copy())
			queueAt(t, outQueueMaxBytes)
			if sendNow(m, copyAt(t, "k", t0.Add(time.Second)), ""); !leftOutAfter(m, t0.Add(time.Second)) {
				t.Fatalf("a mock whose only copy taken is %s, lost, left out none of the tests after it", c.name)
			}
		})
	}
}

// The keys a recording holds copies of are bounded (heldCopiesMax): a key
// past the bound is not kept, so a mock of it counts as needed, while the
// keys kept stay so. A copy requested earlier moves its key's time earlier;
// a later one does not move it. A kind with no ServedByCopy (one never
// registered, or unregistered), and a mock its kind's says it needs in
// itself, are needed whatever the recording holds, and an older recording's
// epoch holds nothing.
func TestTheCopiesARecordingHoldsAreBounded(t *testing.T) {
	servesByCopy(t)
	t0 := time.Now().Add(-time.Minute)
	m := New(nil)
	m.SetOutputChannel(make(chan *models.Mock, 1))
	ep := m.revokeEpoch.Load()
	m.noteDelivered(copyAt(t, "none", t0), ep)     // needs no mock: no key to hold
	m.noteDelivered(copyAt(t, "needed:k", t0), ep) // needed in itself: no copy of k
	if m.neededByNone(copyAt(t, "k", t0.Add(time.Second)), ep) {
		t.Fatal("a mock its kind needs in itself was held as a copy")
	}
	for i := 0; i < heldCopiesMax; i++ {
		m.noteDelivered(copyAt(t, fmt.Sprint(i), t0), ep)
	}
	m.noteDelivered(copyAt(t, "past", t0), ep)
	if m.neededByNone(copyAt(t, "past", t0.Add(time.Second)), ep) {
		t.Fatal("a key past the bound is held")
	}
	if !m.neededByNone(copyAt(t, "0", t0.Add(time.Second)), ep) || !m.neededByNone(copyAt(t, fmt.Sprint(heldCopiesMax-1), t0), ep) {
		t.Fatal("a key within the bound is not held")
	}
	if m.neededByNone(copyAt(t, "0", t0.Add(-time.Second)), ep) {
		t.Fatal("a mock requested before its key's copy is held")
	}
	m.noteDelivered(copyAt(t, "0", t0.Add(time.Hour)), ep)
	if m.neededByNone(copyAt(t, "0", t0.Add(-time.Second)), ep) {
		t.Fatal("a later copy moved its key's time")
	}
	m.noteDelivered(copyAt(t, "0", t0.Add(-2*time.Second)), ep)
	if !m.neededByNone(copyAt(t, "0", t0.Add(-time.Second)), ep) {
		t.Fatal("an earlier copy did not move its key's time")
	}
	if m.neededByNone(copyAt(t, "0", time.Time{}), ep) {
		t.Fatal("a mock with no request time is needed by none")
	}
	if m.neededByNone(copyAt(t, "0", t0.Add(time.Second)), ep-1) {
		t.Fatal("a loss of an older recording's epoch is needed by none")
	}
	m.noteDelivered(copyAt(t, "older", t0), ep-1)
	if m.neededByNone(copyAt(t, "older", t0.Add(time.Second)), ep) {
		t.Fatal("a copy taken for an older recording's channel is held")
	}
	needed := copyAt(t, "", t0.Add(time.Second))
	if m.neededByNone(needed, ep) {
		t.Fatal("a mock its kind's replay needs in itself is needed by none")
	}
	other := copyAt(t, "0", t0.Add(time.Second))
	other.Kind = models.Kind("Unregistered")
	if m.neededByNone(other, ep) {
		t.Fatal("a mock of a kind with no ServedByCopy is needed by none")
	}
	RegisterServedByCopy(copyKind, nil)
	if _, ok := ServedByCopyOf(copyKind); ok || m.neededByNone(copyAt(t, "none", t0), ep) {
		t.Fatal("a kind whose ServedByCopy was unregistered still has one")
	}
}
