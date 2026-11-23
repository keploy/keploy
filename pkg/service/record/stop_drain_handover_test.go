package record

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A frame the drain hands on is the consumer's from then on, which may change
// it (drainOpts.handing). The drain counted a frame handed on after the stop
// as one the agent may have held (heldFrames) by reading its capture time
// after the hand-over: a read that raced a consumer that changes the frame,
// and a count that followed whatever the consumer had made of it. Each of the
// drain's two hand-overs is driven on its own: while it forwards after the
// stop, and once the drain has ended the stream, for what its reader still
// had queued. Each hands on mocks captured before the stop and one captured
// after it, and the consumer re-dates each mock it takes to the other side of
// the stop: the drain must count as held exactly the ones captured before it,
// and never read a mock once the consumer has it (under -race, a DATA RACE
// besides).
func TestForwardUntilDrained_ReadsAFrameOnlyBeforeItsHandOver(t *testing.T) {
	t.Parallel()
	const n = 4
	for _, c := range []struct {
		name              string
		forwarding, ended int
	}{
		{"while forwarding after the stop", n + 1, 0},
		{"after the drain ended the stream", 0, n + 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			before, after := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
			mocks := make([]*models.Mock, 0, c.forwarding+c.ended)
			taken := map[*models.Mock]chan struct{}{}
			for i := 0; i < c.forwarding+c.ended; i++ {
				m := &models.Mock{Kind: models.HTTP, Name: "mocks"}
				m.Spec.ReqTimestampMock = before
				if i == n/2 {
					m.Spec.ReqTimestampMock = after // the one captured after the stop
				}
				mocks = append(mocks, m)
				taken[m] = make(chan struct{})
			}
			stream := make(chan *models.Mock, len(mocks))
			for _, m := range mocks[:c.forwarding] {
				stream <- m
			}
			out := make(chan *models.Mock)
			consumed := make(chan int)
			go func() {
				got := 0
				for m := range out {
					close(taken[m])
					// The consumer's now: it may change the mock as it keeps it.
					if m.Spec.ReqTimestampMock.Equal(after) {
						m.Spec.ReqTimestampMock = before
					} else {
						m.Spec.ReqTimestampMock = after
					}
					got++
				}
				consumed <- got
			}()

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // the stop
			r := &Recorder{logger: zap.NewNop(), frameQuiet: 300 * time.Millisecond}
			forwardUntilDrained(ctx, r, stream, out, make(chan struct{}), drainOpts[*models.Mock]{
				capturedAt: func(m *models.Mock) time.Time {
					// A read of a mock the consumer takes meanwhile is one
					// after its hand-over: waited for, so the drain's reads
					// before the hand-over cost the wait and nothing else.
					select {
					case <-taken[m]:
						t.Errorf("the drain read a mock after handing it to the consumer")
					case <-time.After(20 * time.Millisecond):
					}
					return m.Spec.ReqTimestampMock
				},
				dropped: func(*models.Mock) { t.Errorf("a mock was dropped") },
				closeStream: func() {
					// What the stream's reader still had queued as the drain
					// ended it.
					for _, m := range mocks[c.forwarding:] {
						stream <- m
					}
					close(stream)
				},
				kind: "mocks",
			})
			close(out)
			if got := <-consumed; got != len(mocks) {
				t.Fatalf("the consumer took %d of %d mocks", got, len(mocks))
			}
			if held := r.heldFrames.Load(); held != n {
				t.Fatalf("counted %d mocks as held, of %d captured before the stop and 1 after it: want each counted by its capture time as it was handed over", held, n)
			}
		})
	}
}
