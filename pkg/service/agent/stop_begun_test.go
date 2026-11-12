package agent

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// stopBegunProxy is a Proxy that notes a recording's stop.
type stopBegunProxy struct {
	gracefulProxy
	noted *bool
}

func (p stopBegunProxy) NoteStopBegun() { *p.noted = true }

// The agent hands the recording's stop to its proxy (Proxy.NoteStopBegun);
// one whose proxy cannot note it errors, so the client is told.
func TestNoteStopBegunReachesTheProxy(t *testing.T) {
	noted := false
	a := &Agent{logger: zap.NewNop(), Proxy: stopBegunProxy{noted: &noted}}
	if err := a.NoteStopBegun(context.Background()); err != nil || !noted {
		t.Fatalf("NoteStopBegun = %v, the proxy noted it: %v", err, noted)
	}
	b := &Agent{logger: zap.NewNop(), Proxy: gracefulProxy{}}
	if err := b.NoteStopBegun(context.Background()); err == nil {
		t.Fatal("an agent whose proxy cannot note the stop said it did")
	}
}
