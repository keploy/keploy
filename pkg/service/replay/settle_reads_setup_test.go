package replay

import (
	"context"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// optsInstr is an agent whose Setup notes the options it is started with.
type optsInstr struct {
	*bpNoAgent
	opts []models.SetupOptions
}

func (s *optsInstr) Setup(_ context.Context, _ string, opts models.SetupOptions) error {
	s.opts = append(s.opts, opts)
	return nil
}

// A run that learns noise from the misses it reads (cfg.Test.LearnsNoise:
// enterprise's auto-replays and its sandbox's local replay) starts its agent
// settling reads: the agent's proxy listens marked for the kernel's count from
// its start. An auto-replay that learns none, and every other replay, starts
// the agent as before.
func TestARunThatLearnsNoiseStartsItsAgentSettlingReads(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		learnsNoise, autoReplay bool
	}{
		{"learns noise", true, false},
		{"an auto-replay that learns noise", true, true},
		{"an auto-replay that learns none", false, true},
		{"a plain replay", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newBasePathRun(t, "", "t1")
			run.replayer.config.Test.LearnsNoise = tc.learnsNoise
			run.replayer.config.Test.AutoReplay = tc.autoReplay
			in := &optsInstr{bpNoAgent: run.agent}
			run.replayer.instrument = true
			run.replayer.instrumentation = in
			if _, err := run.replayer.Instrument(context.Background()); err != nil {
				t.Fatalf("Instrument: %v", err)
			}
			if len(in.opts) != 1 {
				t.Fatalf("the agent was set up %d times, want once", len(in.opts))
			}
			if got := in.opts[0].SettleReads; got != tc.learnsNoise {
				t.Fatalf("SettleReads %v, want %v", got, tc.learnsNoise)
			}
		})
	}
}

// With the switch off, a test whose misses keploy could not all read is marked
// MissesUnattributed, and the run's end names its test set, once, with every
// other set so read: nothing may learn noise from them.
func TestTheRunsEndNamesTheTestSetsReadUnattributed(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	core, logs := observer.New(zap.WarnLevel)
	r := &Replayer{logger: zap.New(core), config: &config.Config{}}
	r.config.Test.LearnsNoise = true
	unread := testMisses{asked: true, err: fmt.Errorf("%w: a NAT'd connection", models.ErrUnmatchedCallsUnread)}
	for _, tc := range []struct{ set, name string }{{"set-b", "t1"}, {"set-a", "t2"}, {"set-b", "t3"}} {
		var result models.TestResult
		r.attachReadMockErrors(unread, tc.set, tc.name, &result)
		if !result.FailureInfo.MissesUnattributed {
			t.Fatalf("%s/%s is not marked MissesUnattributed", tc.set, tc.name)
		}
	}
	r.attachReadMockErrors(unread, "set-c", "t4", nil) // a result not kept is still its set's
	r.attachReadMockErrors(testMisses{asked: true}, "set-d", "t5", &models.TestResult{})
	r.sayUnattributed()
	said := logs.FilterMessageSnippet("could not attribute every miss of these test sets to its test, so no noise is learned from them").All()
	if len(said) != 1 {
		t.Fatalf("%d run-end WARNs, want 1", len(said))
	}
	ids, _ := said[0].ContextMap()["ids"].([]interface{})
	got := fmt.Sprint(ids)
	if got != "[set-a set-b set-c]" {
		t.Fatalf("the run's end named %s, want [set-a set-b set-c]", got)
	}
}

// A run that learns no noise names the same sets, without saying noise is not
// learned: it learns none either way.
func TestTheRunsEndNamesTheSetsOfARunThatLearnsNoNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	core, logs := observer.New(zap.WarnLevel)
	r := &Replayer{logger: zap.New(core), config: &config.Config{}}
	r.attachReadMockErrors(testMisses{asked: true, err: fmt.Errorf("%w: lost", models.ErrUnmatchedCallsUnread)}, "set-a", "t1", &models.TestResult{})
	r.sayUnattributed()
	if n := logs.FilterMessageSnippet("so no noise is learned").Len(); n != 0 {
		t.Fatalf("a run that learns no noise said %d times that no noise is learned", n)
	}
	if n := logs.FilterMessageSnippet("a miss may be missing from one of their tests").Len(); n != 1 {
		t.Fatalf("%d run-end WARNs naming the sets, want 1", n)
	}
}
