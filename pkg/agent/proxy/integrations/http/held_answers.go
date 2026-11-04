package http

import (
	"context"
	"sync"
)

// heldAnswers knows, for each recording, the newest of the answers a replay
// served from it and holds (holdUntilClosed). A recording the replay keeps
// for every test (a session or connection lifetime mock, a config mock) is
// served, and held, each time the app asks for it. A hold past the app's FIN
// ends once a later answer of its recording is held: the app, by asking
// again, has moved on from the connection it closed. So of the answers served
// from one recording, at most one holds a connection past its app's FIN;
// every other ends at the FIN, as a hold did before it outlasted the FIN.
// Recordings are told apart by name, as the mock store tells them apart.
type heldAnswers struct {
	newest sync.Map // a recording's name -> its newest *heldAnswer
}

// heldAnswer is one answer held: outlived ends its hold past the app's FIN.
type heldAnswer struct {
	outlived context.CancelFunc
}

// hold registers an answer served from the recording named name, held from
// now on, and ends the hold past the FIN of the answer of that recording
// held before it. The context it returns ends with ctx, or once a later
// answer of the same recording is held; done, called once the hold is over,
// unregisters the answer. An answer of a recording with no name (keploy
// names every recording it writes) is not told from another's: its context
// ends with ctx alone.
func (s *heldAnswers) hold(ctx context.Context, name string) (outlived context.Context, done func()) {
	outlived, cancel := context.WithCancel(ctx)
	if name == "" {
		return outlived, cancel
	}
	mine := &heldAnswer{outlived: cancel}
	if prev, held := s.newest.Swap(name, mine); held {
		prev.(*heldAnswer).outlived()
	}
	return outlived, func() {
		s.newest.CompareAndDelete(name, mine)
		cancel()
	}
}
