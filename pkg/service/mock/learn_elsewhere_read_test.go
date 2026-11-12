package mock

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// elsewhereInstr is a native agent at the round's end whose learn step
// taught recordings from calls made in other windows than theirs, read with
// GetLearnedElsewhere; asked counts the reads.
type elsewhereInstr struct {
	outcomeInstr
	learned []models.MockState
	asked   *int
}

func (e elsewhereInstr) GetLearnedElsewhere(context.Context) ([]models.MockState, error) {
	*e.asked++
	return e.learned, nil
}

// composeElsewhereInstr is a compose agent that left account as it stopped:
// the CLI reads that alone, never the stopped agent.
type composeElsewhereInstr struct {
	elsewhereInstr
	account models.MockOutcome
}

func (c composeElsewhereInstr) ComposeAgentOutcome() (models.MockOutcome, error) {
	return c.account, nil
}

// keploy mock record's round's end writes what record's full round learned
// onto a recording from calls made in other windows than its, from both
// places the agent says it: a native agent's read (GetLearnedElsewhere,
// asked only when the round learns) and, under docker compose, the account
// the agent left as it stopped (the stopped agent is never asked). Its clock
// read is written onto the recording, and the record says it apart. A round
// that read only what was consumed would write the timer's recording with
// nothing learned, every row of the learning still green.
func TestARoundsEndWritesWhatCallsInOtherWindowsTaughtFromTheAgent(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	learned := []models.MockState{{
		Name:     "mock-9",
		Clocks:   map[string]models.ClockOffset{"claimed_at": minute},
		TaughtIn: []models.TeachingWindow{{Test: "o::C", At: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}},
	}}
	detail := replayDetail{set: "s", expected: map[string][]models.MockEntry{"o::B": {{Name: "mock-9"}}}}
	for _, c := range []struct {
		name    string
		compose bool
		learn   bool
	}{
		{"a native agent", false, true},
		{"a compose agent's account", true, true},
		{"a round that does not learn", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Test.Learn = c.learn
			cfg.Mock.Name = "s"
			asked := 0
			native := elsewhereInstr{outcomeInstr: outcomeInstr{clientTestsInstr: clientTestsInstr{reads: true}}, learned: learned, asked: &asked}
			var instr Instrumentation = native
			if c.compose {
				cfg.CommandType = string(utils.DockerCompose)
				instr = composeElsewhereInstr{elsewhereInstr: elsewhereInstr{outcomeInstr: native.outcomeInstr, asked: &asked}, account: models.MockOutcome{Settled: true, LearnedElsewhere: learned}}
			}
			store := &mergingStore{}
			m := &mockService{config: cfg, mockDB: store, logger: zap.NewNop(), instrumentation: instr}
			counts := m.reportOutcome(context.Background(), 1, detail, func() bool { return false })
			if !c.learn {
				if asked != 0 || store.got != nil || counts.learnedElsewhere != nil {
					t.Fatalf("a round that does not learn asked %d times, wrote %+v, said %+v", asked, store.got, counts.learnedElsewhere)
				}
				return
			}
			if counts.learnErr != nil {
				t.Fatal(counts.learnErr)
			}
			if want := map[bool]int{false: 1, true: 0}[c.compose]; asked != want {
				t.Fatalf("asked the agent %d times; want %d", asked, want)
			}
			if _, ok := store.got["mock-9"].Clocks["claimed_at"]; !ok {
				t.Fatalf("the timer's recording was written %+v; want its claimed_at clock read", store.got["mock-9"])
			}
			if len(counts.learnedElsewhere) != 1 || counts.learnedElsewhere[0].Mock != "mock-9" || counts.learnedElsewhere[0].Owner != "o::B" {
				t.Fatalf("the record says %+v as learned across windows; want mock-9, under o::B", counts.learnedElsewhere)
			}
		})
	}
}
