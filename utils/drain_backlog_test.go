package utils

import (
	"context"
	"io"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The rule a stop goes by, look by look, for what the agent still holds from
// before it.
func TestBacklogLooks(t *testing.T) {
	pending := func(p uint64) models.AgentBacklog {
		return models.AgentBacklog{Verdict: models.BacklogPending, Progress: p}
	}
	unanswered := models.AgentBacklog{}

	t.Run("clear and unsupported end the wait, not as a give-up", func(t *testing.T) {
		for _, v := range []models.BacklogVerdict{models.BacklogClear, models.BacklogUnsupported} {
			l := BacklogLooks{Stall: 3}
			if wait, gaveUp := l.Look(models.AgentBacklog{Verdict: v}, nil, 0); wait || gaveUp {
				t.Fatalf("%v: wait=%v gaveUp=%v", v, wait, gaveUp)
			}
		}
	})
	t.Run("the zero answer is cannot tell, never clear", func(t *testing.T) {
		l := BacklogLooks{Stall: 3}
		if wait, _ := l.Look(models.AgentBacklog{}, nil, 0); !wait {
			t.Fatal("an answer that did not come ended the wait")
		}
	})
	t.Run("pending with no progress gives up after Stall looks", func(t *testing.T) {
		l := BacklogLooks{Stall: 3}
		// The first look sets the baseline.
		for i, want := range []bool{true, true, true, false} {
			wait, gaveUp := l.Look(pending(5), nil, 10)
			if wait != want || gaveUp == want {
				t.Fatalf("look %d: wait=%v gaveUp=%v, want wait=%v", i, wait, gaveUp, want)
			}
		}
	})
	t.Run("an agent that never answers gives up after Stall looks", func(t *testing.T) {
		l := BacklogLooks{Stall: 2}
		l.Look(unanswered, nil, 4) // baseline: frames moved from 0
		if wait, _ := l.Look(unanswered, nil, 4); !wait {
			t.Fatal("gave up early")
		}
		if wait, gaveUp := l.Look(unanswered, nil, 4); wait || !gaveUp {
			t.Fatalf("wait=%v gaveUp=%v, want a give-up", wait, gaveUp)
		}
	})
	t.Run("agent progress or a frame resets the count", func(t *testing.T) {
		l := BacklogLooks{Stall: 2}
		frames := uint64(1)
		for i := 0; i < 20; i++ {
			var b models.AgentBacklog
			switch i % 3 {
			case 0:
				b = pending(uint64(i)) // the agent's capture moved
			case 1:
				b = unanswered
				frames++ // a frame came meanwhile
			case 2:
				b = pending(uint64(i - 2)) // unchanged since look i-2: no progress
			}
			if wait, gaveUp := l.Look(b, nil, frames); !wait || gaveUp {
				t.Fatalf("look %d: gave up on a backlog that kept moving", i)
			}
		}
	})
	t.Run("held is what the last answer said", func(t *testing.T) {
		var l BacklogLooks
		l.Stall = 3
		if l.Held() {
			t.Fatal("held before any look")
		}
		for _, b := range []models.AgentBacklog{pending(1), unanswered} {
			l.Look(b, nil, 7)
			if v, _ := l.Said(); !l.Held() || v != b.Verdict {
				t.Fatalf("%v at the last look: held=%v said=%v", b.Verdict, l.Held(), v)
			}
		}
		for _, v := range []models.BacklogVerdict{models.BacklogClear, models.BacklogUnsupported} {
			l.Look(models.AgentBacklog{Verdict: v}, nil, 7)
			if l.Held() {
				t.Fatalf("held after the agent said %v", v)
			}
		}
	})
	t.Run("a look the agent was gone for goes by the look before it", func(t *testing.T) {
		// A real refused dial: its error is the platform's own.
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		_, refused := net.Dial("tcp4", addr)
		if refused == nil {
			t.Fatal("a dial to a closed port connected")
		}
		gone := func(l *BacklogLooks, frames uint64) {
			l.Look(unanswered, refused, frames)
			if !l.LastGone() {
				t.Fatal("a refused look was not taken for the agent gone")
			}
		}
		var l BacklogLooks
		l.Stall = 9
		gone(&l, 1)
		if l.Held() {
			t.Fatal("an agent gone at the only look was taken to hold something")
		}
		// Gone, then an answer, then gone again: the answer in between decides.
		l.Look(pending(2), nil, 1)
		gone(&l, 1)
		if v, ok := l.Said(); !ok || v != models.BacklogPending || !l.Held() {
			t.Fatalf("said %v (%v), held=%v; want the pending answer between the gone looks", v, ok, l.Held())
		}
		// The connection closed in the middle of its answer: gone too.
		l.Look(unanswered, io.ErrUnexpectedEOF, 1)
		if !l.LastGone() {
			t.Fatal("an answer cut off by the agent closing the connection was not taken for the agent gone")
		}
		// A look that timed out is not the agent gone: it may still hold.
		l.Look(unanswered, context.DeadlineExceeded, 1)
		if l.LastGone() || !l.Held() {
			t.Fatalf("a look that timed out was taken for the agent gone (gone=%v held=%v)", l.LastGone(), l.Held())
		}
		for name, before := range map[string]models.AgentBacklog{"said it held something": pending(1), "did not answer in time": unanswered} {
			var l BacklogLooks
			l.Stall = 9
			l.Look(before, nil, 2)
			gone(&l, 2)
			gone(&l, 2) // still gone: the look before both decides
			if !l.Held() {
				t.Fatalf("an agent that %s, then was gone, was taken to hold nothing", name)
			}
			if v, ok := l.Said(); !ok || v != before.Verdict {
				t.Fatalf("said %v (%v), want %v", v, ok, before.Verdict)
			}
		}
		var c BacklogLooks
		c.Stall = 9
		c.Look(models.AgentBacklog{Verdict: models.BacklogClear}, nil, 3)
		gone(&c, 3)
		if c.Held() {
			t.Fatal("an agent that said it held nothing, then was gone, was taken to hold something")
		}
	})
}
