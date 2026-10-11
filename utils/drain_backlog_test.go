package utils

import (
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
			if wait, gaveUp := l.Look(models.AgentBacklog{Verdict: v}, 0); wait || gaveUp {
				t.Fatalf("%v: wait=%v gaveUp=%v", v, wait, gaveUp)
			}
		}
	})
	t.Run("the zero answer is cannot tell, never clear", func(t *testing.T) {
		l := BacklogLooks{Stall: 3}
		if wait, _ := l.Look(models.AgentBacklog{}, 0); !wait {
			t.Fatal("an answer that did not come ended the wait")
		}
	})
	t.Run("pending with no progress gives up after Stall looks", func(t *testing.T) {
		l := BacklogLooks{Stall: 3}
		// The first look sets the baseline.
		for i, want := range []bool{true, true, true, false} {
			wait, gaveUp := l.Look(pending(5), 10)
			if wait != want || gaveUp == want {
				t.Fatalf("look %d: wait=%v gaveUp=%v, want wait=%v", i, wait, gaveUp, want)
			}
		}
	})
	t.Run("an agent that never answers gives up after Stall looks", func(t *testing.T) {
		l := BacklogLooks{Stall: 2}
		l.Look(unanswered, 4) // baseline: frames moved from 0
		if wait, _ := l.Look(unanswered, 4); !wait {
			t.Fatal("gave up early")
		}
		if wait, gaveUp := l.Look(unanswered, 4); wait || !gaveUp {
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
			if wait, gaveUp := l.Look(b, frames); !wait || gaveUp {
				t.Fatalf("look %d: gave up on a backlog that kept moving", i)
			}
		}
	})
}
