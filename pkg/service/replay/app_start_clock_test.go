package replay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	agenthttp "go.keploy.io/server/v3/pkg/platform/http"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// keploy's agent client is what keploy test tells a new app through and
// starts a compose app again through. The run finds both by a type assertion,
// so a signature that drifted from these interfaces would skip the notice or
// the start with no build error.
var (
	_ appStartNotifier  = (*agenthttp.AgentClient)(nil)
	_ composeAppStopper = (*agenthttp.AgentClient)(nil)
)

// appNotice is one notice that the application starts anew.
type appNotice struct {
	set   string
	first time.Time
}

// appNotices notes each AppStartsAnew made of it and answers err.
type appNotices struct {
	noticeMu sync.Mutex
	got      []appNotice
	err      error
}

func (n *appNotices) AppStartsAnew(_ context.Context, set string, first time.Time) error {
	n.noticeMu.Lock()
	defer n.noticeMu.Unlock()
	n.got = append(n.got, appNotice{set, first})
	return n.err
}

func (n *appNotices) notices() []appNotice {
	n.noticeMu.Lock()
	defer n.noticeMu.Unlock()
	return append([]appNotice(nil), n.got...)
}

// noticedInstr is the harness's native instrumentation that takes the notice.
type noticedInstr struct {
	*prInstr
	*appNotices
}

// noticedCompose is the harness's compose instrumentation that takes it,
// each notice noted in the compose order as it comes.
type noticedCompose struct {
	*composeInstr
	*appNotices
}

func (n *noticedCompose) AppStartsAnew(ctx context.Context, set string, first time.Time) error {
	n.order = append(n.order, "notice")
	return n.appNotices.AppStartsAnew(ctx, set, first)
}

// recordedAt puts the harness's cases at the given recorded instants.
func recordedAt(h *prRun, at ...time.Time) {
	for i, tc := range h.cases {
		tc.HTTPReq.Timestamp = at[i]
	}
}

var (
	setLater   = time.Date(2026, 10, 6, 10, 0, 9, 0, time.UTC)
	setEarlier = time.Date(2026, 10, 6, 10, 0, 1, 500000000, time.UTC)
)

// Under the proven switch the agent is told each start of a native set's
// application, after its mocks and before it starts, with the set's earliest
// recorded case (not its first listed), so its clock keeps nothing an
// earlier application read.
func TestANativeSetTellsTheAgentItsApplicationStartsAnew(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	recordedAt(h, setLater, setEarlier)
	n := &appNotices{}
	h.replayer.instrumentation = &noticedInstr{prInstr: h.instr, appNotices: n}
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	got := n.notices()
	if len(got) != 1 || got[0].set != "test-set-0" || !got[0].first.Equal(setEarlier) {
		t.Fatalf("notices %v, want one for test-set-0 at %v", got, setEarlier)
	}
	if h.instr.runs != 1 {
		t.Fatalf("the application started %d times, want once", h.instr.runs)
	}
}

// An application the last set's stop did not end runs on: no notice, so the
// agent keeps the clock it read.
func TestAnApplicationThatRunsOnIsNotToldAnew(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	n := &appNotices{}
	h.replayer.instrumentation = &noticedInstr{prInstr: h.instr, appNotices: n}
	h.replayer.lossAppRunsOn = true
	h.replayer.lossAppRunsOnWhy = "the stop was not waited out"
	h.run(t)
	if got := n.notices(); len(got) != 0 {
		t.Fatalf("notices %v, want none for an application that runs on", got)
	}
}

// With the switch off no clock is kept across cases: nothing is told.
func TestNoNoticeWithTheSwitchOff(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 2, 0)
	n := &appNotices{}
	h.replayer.instrumentation = &noticedInstr{prInstr: h.instr, appNotices: n}
	h.run(t)
	if got := n.notices(); len(got) != 0 {
		t.Fatalf("notices %v, want none with the switch off", got)
	}
}

// A compose stack the set brings up starts its app anew, and so does a kept
// stack's app the last set's stop ended; one found running, or not read, does
// not.
func TestAComposeSetTellsTheAgentOnlyAnAppStartedAnew(t *testing.T) {
	provenOn(t, true)
	for _, tc := range []struct {
		name    string
		stopErr error
		unread  bool
		// runsOn1 and runsOn2: the stop before the first, or the second, set
		// did not end its application (lossAppRunsOn).
		runsOn1, runsOn2 bool
		want             int
	}{
		{"started anew", nil, false, false, false, 2},
		{"found running", errors.New("stop failed"), false, false, false, 1},
		{"not read", nil, true, false, false, 1},
		{"the stack's first app runs on", nil, false, true, false, 1},
		{"the kept stack's app runs on", nil, false, false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &composeInstr{stopErr: tc.stopErr}
			h := composeHarness(t, c)
			recordedAt(h, setLater, setEarlier)
			n := &appNotices{}
			h.replayer.instrumentation = &noticedCompose{composeInstr: c, appNotices: n}
			h.replayer.lossAppRunsOn = tc.runsOn1
			h.runServing(t) // the stack's first set starts its app
			c.unread = tc.unread
			if tc.runsOn2 {
				h.replayer.lossAppRunsOn = true
			}
			h.runServing(t) // the kept stack's app starts again, or not anew
			got := n.notices()
			if len(got) != tc.want {
				t.Fatalf("%d notices %v, want %d", len(got), got, tc.want)
			}
			for _, g := range got {
				if !g.first.Equal(setEarlier) {
					t.Fatalf("notice %v, want the set's earliest case at %v", g, setEarlier)
				}
			}
		})
	}
}

// A kept stack's app started anew is told to the agent before its start, as
// a stack the set brings up is, once the agent is ready for the set: the new
// process's first clock read is then at the set's first case. Told after the
// start, the app could read the later instant only the app before it saw,
// and the agent's move to the set's first case then put its clock back.
func TestAKeptStacksAppStartedAnewIsToldBeforeItStarts(t *testing.T) {
	provenOn(t, true)
	c := &composeInstr{}
	h := composeHarness(t, c)
	n := &appNotices{}
	h.replayer.instrumentation = &noticedCompose{composeInstr: c, appNotices: n}
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("the stack's first set: status %s, want PASSED", status)
	}
	c.order = nil
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("the kept stack's set: status %s, want PASSED", status)
	}
	if got := strings.Join(c.order, ","); got != "agent ready,notice,start" || len(n.notices()) != 2 {
		t.Fatalf("the kept stack's set: %s with %d notices; want agent ready,notice,start: told once the agent is ready, before the start", got, len(n.notices()))
	}
}

// A notice the agent cannot take is said, with its next step; the set runs on.
func TestANoticeTheAgentCannotTakeIsSaid(t *testing.T) {
	provenOn(t, true)
	core, logs := observer.New(zapcore.WarnLevel)
	r := &Replayer{logger: zap.New(core), instrumentation: &noticedInstr{prInstr: &prInstr{}, appNotices: &appNotices{err: errors.New("agent gone")}}}
	r.tellAppStartsAnew(context.Background(), "test-set-0", nil)
	got := logs.FilterMessageSnippet("could not be told the application starts anew").All()
	if len(got) != 1 || got[0].ContextMap()["next_step"] == nil || got[0].ContextMap()["test-set"] != "test-set-0" {
		t.Fatalf("logs %v, want one warning naming the set and a next step", logs.All())
	}
	// A run that stopped says nothing: the notice died with it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.tellAppStartsAnew(ctx, "test-set-0", nil)
	if n := len(logs.FilterMessageSnippet("could not be told").All()); n != 1 {
		t.Fatalf("%d warnings after a stopped run's notice, want still 1", n)
	}
}

// A notice the agent takes is not said: it carries the set's earliest case.
func TestANoticeTheAgentTakesIsNotSaid(t *testing.T) {
	provenOn(t, true)
	core, logs := observer.New(zapcore.DebugLevel)
	n := &appNotices{}
	r := &Replayer{logger: zap.New(core), instrumentation: &noticedInstr{prInstr: &prInstr{}, appNotices: n}}
	tc := &models.TestCase{Kind: models.HTTP}
	tc.HTTPReq.Timestamp = setEarlier
	r.tellAppStartsAnew(context.Background(), "test-set-0", []*models.TestCase{tc})
	if got := n.notices(); len(got) != 1 || !got[0].first.Equal(setEarlier) || logs.Len() != 0 {
		t.Fatalf("notices %v, logs %v; want one at %v and nothing said", got, logs.All(), setEarlier)
	}
}

// An instrumentation that cannot take the notice is not told.
func TestNoNoticeToAnInstrumentationWithoutIt(t *testing.T) {
	provenOn(t, true)
	core, logs := observer.New(zapcore.DebugLevel)
	r := &Replayer{logger: zap.New(core), instrumentation: &prInstr{}}
	r.tellAppStartsAnew(context.Background(), "test-set-0", nil)
	if logs.Len() != 0 {
		t.Fatalf("logs %v, want none", logs.All())
	}
}

func TestEarliestRecordedReadsEachCaseByItsKind(t *testing.T) {
	grpc := &models.TestCase{Kind: models.GRPC_EXPORT}
	grpc.GrpcReq.Timestamp = setEarlier
	grpc.HTTPReq.Timestamp = setEarlier.Add(-time.Hour) // not a gRPC case's request
	httpCase := &models.TestCase{Kind: models.HTTP}
	httpCase.HTTPReq.Timestamp = setLater
	none := &models.TestCase{Kind: models.HTTP}
	for _, tc := range []struct {
		name string
		tcs  []*models.TestCase
		want time.Time
	}{
		{"the earliest of both kinds", []*models.TestCase{httpCase, nil, grpc, none}, setEarlier},
		{"an HTTP case", []*models.TestCase{none, httpCase}, setLater},
		{"none recorded", []*models.TestCase{none, nil}, time.Time{}},
		{"none at all", nil, time.Time{}},
	} {
		if got := earliestRecorded(tc.tcs); !got.Equal(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// appStartsAnew says whether the application starts anew.
func TestAppStartsAnewSaysWhetherItDoes(t *testing.T) {
	r := &Replayer{}
	if !r.appStartsAnew() {
		t.Fatal("a start with nothing running on is not anew")
	}
	r.lossAppRunsOn = true
	if r.appStartsAnew() {
		t.Fatal("an application that runs on starts anew")
	}
	if !r.appStartsAnew() {
		t.Fatal("the start after the one that ran on is not anew")
	}
}
