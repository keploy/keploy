package manager

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestSendMapping_NeverDropsWhenRecorderIsSlow pins the guarantee behind the
// go-memory-load-mongo "no_mocks" flake.
//
// The recorder rewrites the whole of mappings.yaml to persist a mapping, so it
// drains this channel in bursts, not continuously. The send used to be a bare
// `select { case ch <- m: default: }` — the moment the buffer was full the
// mapping was thrown away, with nothing logged. Those tests then had no mocks
// attributed to them and replay reported no_mocks/candidates:0 for exactly them.
//
// A slow recorder must cost latency, never data.
func TestSendMapping_NeverDropsWhenRecorderIsSlow(t *testing.T) {
	const total = 500 // well past any buffer the recorder gives us

	m := &SyncMockManager{}
	// A deliberately small buffer: the real one is 100 and a heavy recording
	// overruns it. Anything that fits trivially would not exercise the overflow.
	ch := make(chan models.TestMockMapping, 4)
	m.SetMappingChannel(context.Background(), ch)

	// The recorder: reads in bursts with stalls between, as a batching writer does.
	got := make(chan []string, 1)
	go func() {
		names := make([]string, 0, total)
		for m := range ch {
			names = append(names, m.TestName)
			if len(names)%32 == 0 {
				time.Sleep(2 * time.Millisecond) // the file rewrite
			}
		}
		got <- names
	}()

	want := make([]string, 0, total)
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("post-orders-%d", i)
		want = append(want, name)
		m.sendMapping(ch, models.TestMockMapping{TestName: name, MockIDs: []string{"mock-" + name}})
	}

	// Let the overflow drainer finish handing over its backlog.
	deadline := time.After(15 * time.Second)
	for {
		m.mappingOverflowMu.Lock()
		left, draining := len(m.mappingOverflow), m.mappingDraining
		m.mappingOverflowMu.Unlock()
		if left == 0 && !draining {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("overflow never drained: %d mappings still queued", left)
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(ch)

	var names []string
	select {
	case names = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("recorder never finished")
	}

	if len(names) != total {
		t.Fatalf("recorder received %d of %d mappings: a mapping the agent resolved but "+
			"could not hand over must be queued, never discarded — every dropped one is a "+
			"test that replays as no_mocks", len(names), total)
	}
	// Order matters: the recorder upserts by test name, and out-of-order delivery
	// would let a stale entry overwrite a newer one.
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("mapping %d delivered out of order: got %q want %q", i, names[i], want[i])
		}
	}
}

// TestSendMapping_StopsWhenStreamIsGone asserts the drainer cannot outlive the
// recorder's stream. Without the stream-ctx escape a blocking hand-off to a
// channel nobody reads would wedge the goroutine forever.
func TestSendMapping_StopsWhenStreamIsGone(t *testing.T) {
	m := &SyncMockManager{}
	ch := make(chan models.TestMockMapping) // unbuffered, never read
	ctx, cancel := context.WithCancel(context.Background())
	m.SetMappingChannel(ctx, ch)

	for i := 0; i < 10; i++ {
		m.sendMapping(ch, models.TestMockMapping{TestName: fmt.Sprintf("t-%d", i)})
	}

	cancel() // the recorder disconnected

	deadline := time.After(5 * time.Second)
	for {
		m.mappingOverflowMu.Lock()
		draining := m.mappingDraining
		m.mappingOverflowMu.Unlock()
		if !draining {
			return
		}
		select {
		case <-deadline:
			t.Fatal("overflow drainer did not stop after the mapping stream went away")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
