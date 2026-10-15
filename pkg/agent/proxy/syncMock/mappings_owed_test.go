package manager

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A test's mapping goes on the mapping stream after its mocks go on the mock
// stream. Until it is on it -- queued behind a recorder that has not taken the
// ones before it, or not yet sent at all -- a recording's stop waits for it:
// the agent answered "clear" once the mocks were out, and a stop whose mapping
// drain ended on quiet lost the mapping of a test it recorded (replay then
// serves that test no mocks). Asked about a time before the test, it is not
// pending: what the app did after a stop does not hold the stop.
func TestBacklogBefore_AMappingNotYetOnTheStreamIsPending(t *testing.T) {
	m := New(nil)
	out := make(chan *models.Mock, 16)
	m.SetOutputChannel(out)
	defer m.AddWatermark(&backlogWatermark{})() // every parser caught up
	m.SetFirstRequestSignaled()
	// A recorder that has not taken a mapping yet: nothing reads the stream.
	mappings := make(chan models.TestMockMapping)
	m.SetMappingChannel(context.Background(), mappings)

	start := time.Now().Add(-2 * time.Second)
	end := start.Add(100 * time.Millisecond)
	m.AddMock(&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		ReqTimestampMock: start.Add(10 * time.Millisecond),
		ResTimestampMock: start.Add(20 * time.Millisecond),
	}})
	m.ResolveRange(start, end, "test-1", true, true)
	if len(out) != 1 {
		t.Fatalf("setup: the test's mock was not handed on (%d on the mock stream)", len(out))
	}

	stop := time.Now()
	st := m.BacklogBefore(stop)
	if !st.Pending || st.WaitingOn == "" {
		t.Fatalf("%+v: a test's mapping from before the stop, not yet on the mapping stream, is not pending", st)
	}
	if st := m.BacklogBefore(start.Add(-time.Second)); st.Pending {
		t.Fatalf("%+v: pending for a time before the test", st)
	}

	select {
	case got := <-mappings:
		if got.TestName != "test-1" {
			t.Fatalf("the mapping stream carried %+v, want test-1's", got)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the mapping never came")
	}
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for m.BacklogBefore(stop).Pending {
		if time.Now().After(deadline) {
			t.Fatalf("%+v: still pending once the mapping is on the stream", m.BacklogBefore(stop))
		}
		time.Sleep(time.Millisecond)
	}
}

// A mapping lost on the way -- its stream gone with it queued -- is said to
// be lost, and holds no stop.
func TestBacklogBefore_AMappingLostWithItsStreamIsNotPending(t *testing.T) {
	m := New(nil)
	out := make(chan *models.Mock, 16)
	m.SetOutputChannel(out)
	defer m.AddWatermark(&backlogWatermark{})()
	m.SetFirstRequestSignaled()
	streamCtx, endStream := context.WithCancel(context.Background())
	m.SetMappingChannel(streamCtx, make(chan models.TestMockMapping))

	start := time.Now().Add(-2 * time.Second)
	m.AddMock(&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		ReqTimestampMock: start.Add(10 * time.Millisecond),
		ResTimestampMock: start.Add(20 * time.Millisecond),
	}})
	m.ResolveRange(start, start.Add(100*time.Millisecond), "test-1", true, true)
	stop := time.Now()
	if !m.BacklogBefore(stop).Pending {
		t.Fatal("setup: the queued mapping is not pending")
	}
	endStream()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for m.BacklogBefore(stop).Pending {
		if time.Now().After(deadline) {
			t.Fatalf("%+v: a mapping lost with its stream still holds the stop", m.BacklogBefore(stop))
		}
		time.Sleep(time.Millisecond)
	}
}

// A mapping queued for a mapping stream its recorder replaced (a reconnect,
// or a new recording) holds no stop: the stream it was owed to has gone with
// its recording. Before, its drainer, blocked on the old stream that nobody
// reads, kept the debt, and every stop after said "pending" until its stall
// bound. A mapping for the new stream is owed to it as before.
func TestBacklogBefore_AMappingOwedToAReplacedStreamIsNotPending(t *testing.T) {
	m := New(nil)
	out := make(chan *models.Mock, 16)
	m.SetOutputChannel(out)
	defer m.AddWatermark(&backlogWatermark{})()
	m.SetFirstRequestSignaled()
	// The old stream: nobody reads it, and it is never ended.
	m.SetMappingChannel(context.Background(), make(chan models.TestMockMapping))

	resolve := func(name string, start time.Time) {
		t.Helper()
		before := len(out)
		m.AddMock(&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: start.Add(10 * time.Millisecond),
			ResTimestampMock: start.Add(20 * time.Millisecond),
		}})
		m.ResolveRange(start, start.Add(100*time.Millisecond), name, true, true)
		if len(out) != before+1 {
			t.Fatalf("setup: %s's mock was not handed on", name)
		}
	}
	resolve("test-old", time.Now().Add(-4*time.Second))
	stop := time.Now()
	if !m.BacklogBefore(stop).Pending {
		t.Fatal("setup: the mapping queued for the old stream is not pending")
	}

	mappings := make(chan models.TestMockMapping)
	m.SetMappingChannel(context.Background(), mappings)
	if st := m.BacklogBefore(stop); st.Pending {
		t.Fatalf("%+v: a mapping owed to a replaced stream holds the stop", st)
	}

	resolve("test-new", time.Now().Add(-2*time.Second))
	stop = time.Now()
	if !m.BacklogBefore(stop).Pending {
		t.Fatal("a mapping owed to the new stream, not yet on it, is not pending")
	}
	select {
	case got := <-mappings:
		if got.TestName != "test-new" {
			t.Fatalf("the new stream carried %+v, want test-new's", got)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the new stream's mapping never came")
	}
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for m.BacklogBefore(stop).Pending {
		if time.Now().After(deadline) {
			t.Fatalf("%+v: still pending once the new stream's mapping is on it", m.BacklogBefore(stop))
		}
		time.Sleep(time.Millisecond)
	}
}

// A mapping a sender read the old stream for, sent once the stream was
// replaced (SetMappingChannel came between the read and the send), does not
// hold the new stream's mappings behind it. Before, it started a drainer on
// the old stream, which nobody reads, under the new stream's generation, and
// every mapping of the new stream queued behind it: lost, and (owed) holding
// each of its stops "pending" until the stall bound.
func TestAMappingForAReplacedStreamDoesNotHoldTheNewOnes(t *testing.T) {
	m := New(nil)
	out := make(chan *models.Mock, 16)
	m.SetOutputChannel(out)
	defer m.AddWatermark(&backlogWatermark{})()
	m.SetFirstRequestSignaled()
	old := make(chan models.TestMockMapping) // nobody reads it
	m.SetMappingChannel(context.Background(), old)
	mappings := make(chan models.TestMockMapping)
	m.SetMappingChannel(context.Background(), mappings)

	// The send of a mapping read for the old stream, after the swap.
	paid := false
	m.sendMapping(old, models.TestMockMapping{TestName: "test-old"}, func() { paid = true })
	if !paid {
		t.Fatal("a mapping for a replaced stream kept its debt")
	}

	start := time.Now().Add(-2 * time.Second)
	m.AddMock(&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		ReqTimestampMock: start.Add(10 * time.Millisecond),
		ResTimestampMock: start.Add(20 * time.Millisecond),
	}})
	m.ResolveRange(start, start.Add(100*time.Millisecond), "test-new", true, true)
	stop := time.Now()
	select {
	case got := <-mappings:
		if got.TestName != "test-new" {
			t.Fatalf("the new stream carried %+v, want test-new's", got)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatalf("the new stream's mapping never came: held behind the replaced stream's (%+v)", m.BacklogBefore(stop))
	}
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for m.BacklogBefore(stop).Pending {
		if time.Now().After(deadline) {
			t.Fatalf("%+v: still pending once the new stream's mapping is on it", m.BacklogBefore(stop))
		}
		time.Sleep(time.Millisecond)
	}
}
