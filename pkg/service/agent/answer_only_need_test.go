package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// answeringProxy is wireProxy that answers variables as recorded
// (coreAgent.ExactTestAnswerer), keeping each hand: "<test>: <names>".
type answeringProxy struct {
	*wireProxy
	mu     sync.Mutex
	handed []string
	fail   error
}

func (p *answeringProxy) AnswerAsRecordedAtOpen(test string, names []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handed = append(p.handed, test+": "+strings.Join(names, ","))
	return p.fail
}

func (p *answeringProxy) hands() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.handed
	p.handed = nil
	return out
}

// A test that only receives values earlier tests make, in answers (the CI
// test's list of every app the tests before it made), runs although one of
// those tests has not run before it: the agent hands its proxy, before the
// test opens, the values of each such test that has not, to answer as
// recorded, and says so in one line per such test. A test that sends such a
// value, beside or instead of receiving it, is set aside as before, and so is
// one whose proxy cannot answer as recorded. A test set aside, and a record,
// hand nothing; a test with no such need hands none, dropping what an earlier
// begin of it left.
func TestAnAnswerOnlyNeedRunsTheTestAndAnswersAsRecorded(t *testing.T) {
	ctx := context.Background()
	const slow, made, gone, gone2 = "run-appId0-13", "run-appId0-7", "run-appId0-9", "run-appId0-11"
	whySlow := "keploy: set aside: o::TestCI uses a value o::TestSlow sends (run-appId0-13, 8 characters), and o::TestSlow has not run before it in this replay"
	whySends := "keploy: set aside: o::TestSends uses a value o::TestSlow sends (run-appId0-13, 8 characters), and o::TestSlow has not run before it in this replay"
	needs := map[string][]models.ScopeNeed{
		"o::TestCI": {
			{Test: "o::TestMake", Reason: "make", Answers: []string{made}},
			{Test: "o::TestSlow", Reason: whySlow, Answers: []string{slow}},
		},
		"o::TestSends":    {{Test: "o::TestSlow", Reason: whySends}},
		"o::TestBoth":     {{Test: "o::TestSlow", Reason: "receives", Answers: []string{slow}}, {Test: "o::TestSlow", Reason: "sends"}},
		"o::TestGated":    {{Test: "o::TestSlow", Reason: "receives", Answers: []string{slow}}},
		"o::TestNoSender": {{Reason: "nosender", Answers: []string{gone2, gone}}},
		"o::TestOneGone":  {{Reason: "nosender", Answers: []string{gone}}},
	}
	setup := func(t *testing.T, mode models.Mode, answers bool) (*Agent, *answeringProxy, *observer.ObservedLogs) {
		t.Helper()
		a, wp := wireAgent(t, mode)
		ap := &answeringProxy{wireProxy: wp}
		if answers {
			a.Proxy = ap
		}
		core, logs := observer.New(zap.InfoLevel)
		a.logger = zap.New(core)
		claimSelf(t, a)
		require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestGated": "listed"}, needs))
		return a, ap, logs
	}
	run := func(t *testing.T, a *Agent, name string) models.ScopeBeginResp {
		t.Helper()
		b, serr := a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		if b.Action == "" {
			_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
			require.Nil(t, serr)
		}
		return b
	}
	answered := func(logs *observer.ObservedLogs) []string {
		var out []string
		for _, e := range logs.FilterMessageSnippet("only in answers").All() {
			out = append(out, e.Message)
		}
		return out
	}

	t.Run("a proxy that answers as recorded", func(t *testing.T) {
		a, ap, logs := setup(t, models.MODE_TEST, true)
		require.Empty(t, run(t, a, "TestMake").Action)
		require.Equal(t, []string{"o::TestMake: "}, ap.hands(), "a test with no such need hands none")
		require.Empty(t, run(t, a, "TestCI").Action, "a test that only receives the values of a test not run runs")
		require.Equal(t, []string{"o::TestCI: " + slow}, ap.hands(), "only the values of the test that has not run are handed")
		require.Equal(t, []string{"keploy: o::TestCI receives 1 value o::TestSlow makes (" + slow + ") only in answers; o::TestSlow has not run before it in this replay, so it is answered as recorded"},
			answered(logs))
		require.Empty(t, run(t, a, "TestNoSender").Action, "a test that only receives values no recording sends runs")
		require.Equal(t, []string{"o::TestNoSender: " + gone2 + "," + gone}, ap.hands())
		require.Empty(t, run(t, a, "TestOneGone").Action)
		require.Equal(t, []string{"o::TestOneGone: " + gone}, ap.hands())
		require.Equal(t, []string{
			"keploy: o::TestNoSender receives 2 values that no recording in this run sends (" + gone2 + ", " + gone + ") only in answers, so they are answered as recorded",
			"keploy: o::TestOneGone receives 1 value that no recording in this run sends (" + gone + ") only in answers, so it is answered as recorded",
		}, answered(logs)[1:], "a need that names no test reads as values no recording sends")
		require.Equal(t, models.ScopeBeginResp{V: 1, Status: "ok", Action: models.ScopeActionSkip, Reason: whySends}, run(t, a, "TestSends"), "a test that sends the value is set aside")
		require.Equal(t, "sends", run(t, a, "TestBoth").Reason, "a test that sends the value beside receiving it is set aside")
		require.Equal(t, "listed", run(t, a, "TestGated").Reason)
		require.Empty(t, ap.hands(), "a test set aside hands nothing")
		require.Len(t, answered(logs), 3, "a test set aside does not say it is answered as recorded")
		require.Equal(t, models.ScopeOutcomeUnmet, windowOf(t, a, "o::TestSends").Outcome)
	})

	t.Run("a proxy that cannot", func(t *testing.T) {
		a, _, logs := setup(t, models.MODE_TEST, false)
		require.Empty(t, run(t, a, "TestMake").Action, "a test with no such need runs")
		require.Equal(t, models.ScopeBeginResp{V: 1, Status: "ok", Action: models.ScopeActionSkip, Reason: whySlow}, run(t, a, "TestCI"), "fail closed: the test waits on the test not run")
		require.Equal(t, "nosender", run(t, a, "TestNoSender").Reason, "fail closed: a test that only receives values no recording sends is set aside")
		require.Empty(t, answered(logs))
		require.Equal(t, models.ScopeOutcomeUnmet, windowOf(t, a, "o::TestCI").Outcome)
	})

	t.Run("a proxy that fails to take them", func(t *testing.T) {
		a, ap, logs := setup(t, models.MODE_TEST, true)
		ap.fail = errors.New("no manager")
		require.Empty(t, run(t, a, "TestMake").Action)
		require.Equal(t, whySlow, run(t, a, "TestCI").Reason, "fail closed: the test waits on the test not run")
		require.Empty(t, answered(logs))
	})

	// The line is said only once the test began: a begin refused after the
	// hand (the test process does not carry keploy's shim, or the proxy
	// refuses to serve the test) says nothing, and the next begin that
	// opens the test says it once. Mutation: say it at the hand.
	t.Run("a begin that fails after the hand", func(t *testing.T) {
		a, ap, logs := setup(t, models.MODE_TEST, true)
		h := &shimHooks{armed: map[int]bool{}}
		a.Hooks = h
		_, serr := a.ProvenBegin(ctx, begin("TestCI"))
		require.NotNil(t, serr, "the unshimmed begin is refused")
		require.Equal(t, models.ScopeErrNotInRun, serr.Code)
		require.Equal(t, []string{"o::TestCI: " + slow + "," + made}, ap.hands(), "the hand comes before the refusal")
		require.Empty(t, answered(logs), "a refused begin says it is answered as recorded")
		h.armed[os.Getpid()] = true
		ap.mu.Lock()
		ap.setErr = errors.New("the proxy refused the test's mocks")
		ap.mu.Unlock()
		_, serr = a.ProvenBegin(ctx, begin("TestCI"))
		require.NotNil(t, serr, "the begin the proxy refuses fails")
		require.Contains(t, serr.Message, "the proxy refused the test's mocks")
		require.Empty(t, answered(logs), "a begin that failed to open says it is answered as recorded")
		ap.mu.Lock()
		ap.setErr = nil
		ap.mu.Unlock()
		require.Empty(t, run(t, a, "TestCI").Action)
		require.Len(t, answered(logs), 2, "the begin that opens the test says each test it does not wait on, once")
	})

	t.Run("a record", func(t *testing.T) {
		a, ap, logs := setup(t, models.MODE_RECORD, true)
		require.Empty(t, run(t, a, "TestCI").Action)
		require.Empty(t, ap.hands(), "a record hands nothing")
		require.Empty(t, answered(logs))
	})
}
