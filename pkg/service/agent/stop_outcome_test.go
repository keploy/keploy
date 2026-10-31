package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// outcomeProxy answers the two end-of-run reads. block makes the consumed read
// never return, as a wedged proxy would.
type outcomeProxy struct {
	coreAgent.Proxy
	consumed    []models.MockState
	consumedErr error
	missed      []models.UnmatchedCall
	block       chan struct{}
	missedAsked chan struct{} // closed when GetMockErrors is called, if set
}

func (p *outcomeProxy) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	if p.block != nil {
		<-p.block
	}
	return p.consumed, p.consumedErr
}

func (p *outcomeProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	if p.missedAsked != nil {
		close(p.missedAsked)
	}
	return p.missed, nil
}

// Only an agent in a container serving a mock replay leaves an outcome. A
// native agent is asked over HTTP before it is stopped, and writing into the
// host's /tmp from it would be a file nobody reads, in a directory other users
// share.
func TestStopOutcomePathOnlyForAContainerisedMockReplay(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts models.SetupOptions
		want string
	}{
		{"docker mock replay", models.SetupOptions{IsDocker: true, MockMode: true, Mode: models.MODE_TEST}, kdocker.AgentOutcomeFile},
		{"native mock replay", models.SetupOptions{MockMode: true, Mode: models.MODE_TEST}, ""},
		{"docker mock record", models.SetupOptions{IsDocker: true, MockMode: true, Mode: models.MODE_RECORD}, ""},
		{"docker keploy test", models.SetupOptions{IsDocker: true, Mode: models.MODE_TEST}, ""},
	} {
		if got := stopOutcomePath(tc.opts); got != tc.want {
			t.Errorf("%s: stopOutcomePath = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// What the agent leaves is exactly what /consumedmocks and /mockerrors would
// have answered, in the one document the CLI decodes. Under the proven switch
// the agent also finishes its end of the run first, and the account says it
// did (Settled); with the switch off it does not say so.
// TestWriteStopOutcomeSettlesTheRunUnderTheSwitch checks the settle itself.
func TestWriteStopOutcomeLeavesWhatTheReplayServedAndMissed(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch %v", on), func(t *testing.T) {
			t.Setenv(provenswitch.Env, strconv.FormatBool(on))
			// The default hooks, whose end of the run succeeds, whatever
			// an earlier test left in place.
			prev := ActiveHooks
			ActiveHooks = &AgentHook{}
			t.Cleanup(func() { ActiveHooks = prev })
			writeStopOutcomeLeavesWhatTheReplayServedAndMissed(t, on)
		})
	}
}

func writeStopOutcomeLeavesWhatTheReplayServedAndMissed(t *testing.T, on bool) {
	proxy := &outcomeProxy{
		consumed: []models.MockState{{Name: "mock-0", Kind: models.HTTP}, {Name: "mock-3"}},
		missed:   []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /price/tsla", Destination: "dep:9411"}},
	}
	a := &Agent{Proxy: proxy, logger: zap.NewNop()}
	dir := t.TempDir()
	path := filepath.Join(dir, "outcome.json")

	if err := a.writeStopOutcome(context.Background(), path); err != nil {
		t.Fatalf("writeStopOutcome: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no outcome left: %v", err)
	}
	var got models.MockOutcome
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the outcome is not a models.MockOutcome: %v\n%s", err, raw)
	}
	want := models.MockOutcome{Consumed: proxy.consumed, Missed: proxy.missed, Settled: on}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("left %+v, want %+v", got, want)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left %d files, want only the outcome: the temporary file was not renamed away", len(entries))
	}
}

// An agent that cannot read what it served leaves NOTHING -- the CLI reads a
// missing file as "unknown", which is the truth. An empty outcome would read
// as a replay that served and missed nothing, and replace a previous one.
func TestWriteStopOutcomeLeavesNothingItCouldNotRead(t *testing.T) {
	a := &Agent{Proxy: &outcomeProxy{consumedErr: errors.New("mock manager not found")}, logger: zap.NewNop()}
	path := filepath.Join(t.TempDir(), "outcome.json")
	if err := a.writeStopOutcome(context.Background(), path); err == nil {
		t.Fatal("writeStopOutcome succeeded without the mocks served")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("left an outcome it could not read: %v", err)
	}
}

// leaveStopOutcome runs ahead of the agent's shutdown, on the signal goroutine.
// A proxy that never answers must cost the shutdown stopOutcomeTimeout, not
// the rest of compose's stop grace -- after which the container is killed and
// neither the outcome nor the agent's own teardown happens.
//
// And giving up has to mean that no outcome is left: a proxy that answers
// after all, as the shutdown goes on, must not have its answer written in
// behind the give-up, for the CLI to read as this replay's account.
func TestLeaveStopOutcomeGivesUpOnAWedgedProxy(t *testing.T) {
	block := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(block)
		}
	}()
	proxy := &outcomeProxy{block: block, missedAsked: make(chan struct{})}
	a := &Agent{Proxy: proxy, logger: zap.NewNop()}
	dir := t.TempDir()
	path := filepath.Join(dir, "outcome.json")

	done := make(chan struct{})
	go func() {
		a.leaveStopOutcome(path)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopOutcomeTimeout + 2*time.Second):
		t.Fatalf("leaveStopOutcome was still waiting on a wedged proxy after %s", stopOutcomeTimeout+2*time.Second)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("left an outcome for a proxy that never answered: %v", err)
	}

	// The proxy answers after all.
	close(block)
	released = true
	select {
	case <-proxy.missedAsked:
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned read never finished")
	}
	// Longer than the abandoned read takes to write, were it going to.
	time.Sleep(500 * time.Millisecond)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("wrote %v after giving up (%v)", entries, err)
	}
}

// The account is armed at Setup: whatever a previous run left is cleared -- a
// restarted container keeps its /tmp, and the last run's account would stand
// in for this one -- and this run's is written when the agent is stopped.
func TestArmStopOutcomeClearsTheLastRunsAndLeavesThisOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.json")
	if err := os.WriteFile(path, []byte(`{"consumed":[{"name":"last-run"}],"missed":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := &outcomeProxy{consumed: []models.MockState{{Name: "this-run"}}}
	a := &Agent{Proxy: proxy, logger: zap.NewNop()}
	var onStop []func()
	a.armStopOutcome(path, func(fn func()) { onStop = append(onStop, fn) })

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the last run's outcome is still there to be read as this one's: %v", err)
	}
	if len(onStop) != 1 {
		t.Fatalf("registered %d stop hooks, want 1", len(onStop))
	}
	onStop[0]()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stopped, the agent left no outcome: %v", err)
	}
	var got models.MockOutcome
	if err := json.Unmarshal(raw, &got); err != nil || len(got.Consumed) != 1 || got.Consumed[0].Name != "this-run" {
		t.Fatalf("left %s (%v), want this run's outcome", raw, err)
	}
}

// The outcome replaces the file at path whole -- a new file renamed into
// place -- rather than rewriting it: an agent killed while writing in place
// leaves half of one, which reads as a parse error at best. A second name for
// the file already there shows which happened: a rename leaves it holding
// what it held.
func TestWriteStopOutcomeReplacesTheFileWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outcome.json")
	const stale = `{"consumed":[],"missed":[]}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "as-it-was")); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Proxy: &outcomeProxy{consumed: []models.MockState{{Name: "mock-0"}}}, logger: zap.NewNop()}
	if err := a.writeStopOutcome(context.Background(), path); err != nil {
		t.Fatalf("writeStopOutcome: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "as-it-was")); string(raw) != stale {
		t.Fatalf("the file at path was rewritten in place (it now holds %s), not replaced", raw)
	}
	if raw, _ := os.ReadFile(path); !json.Valid(raw) || string(raw) == stale {
		t.Fatalf("path holds %s, want the new outcome", raw)
	}
}

// Under compose the CLI can no longer ask a stopped agent which test ran when,
// or with what verdict, so the account it leaves carries the windows too.
func TestWriteStopOutcomeCarriesTheTestsWindows(t *testing.T) {
	a := &Agent{Proxy: &outcomeProxy{}, logger: zap.NewNop()}
	ctx := context.Background()
	if err := a.BeginScope(ctx, "TestOrders", 4); err != nil {
		t.Fatal(err)
	}
	a.NoteScopeOutcome("TestOrders", 4, models.ScopeOutcomePassed)
	if err := a.EndScope(ctx, "TestOrders", 4); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "outcome.json")
	if err := a.writeStopOutcome(ctx, path); err != nil {
		t.Fatalf("writeStopOutcome: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var got models.MockOutcome
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Windows) != 1 || got.Windows[0].Name != "TestOrders" || got.Windows[0].Outcome != models.ScopeOutcomePassed {
		t.Fatalf("the outcome carries windows %+v, want TestOrders passed", got.Windows)
	}
}

// partialMissProxy is an outcomeProxy whose read of the calls no mock answered
// was incomplete (the proven switch): it returns what it read, and says so.
type partialMissProxy struct{ outcomeProxy }

func (p *partialMissProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return p.missed, fmt.Errorf("%w: keploy's error drain stopped", models.ErrUnmatchedCallsUnread)
}

// An incomplete read of the misses is kept, with what was read and why it may
// be short, rather than throwing away the whole account (what was served
// included).
func TestWriteStopOutcomeKeepsAnIncompleteMissRead(t *testing.T) {
	proxy := &partialMissProxy{outcomeProxy{
		consumed: []models.MockState{{Name: "mock-0", Kind: models.HTTP}},
		missed:   []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /a"}},
	}}
	a := &Agent{Proxy: proxy, logger: zap.NewNop()}
	path := filepath.Join(t.TempDir(), "outcome.json")
	if err := a.writeStopOutcome(context.Background(), path); err != nil {
		t.Fatalf("writeStopOutcome: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no outcome left: %v", err)
	}
	var got models.MockOutcome
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("not a models.MockOutcome: %v", err)
	}
	// The cause alone: whoever reads the account names the incomplete read
	// itself, so the sentinel's text is not said twice.
	if len(got.Consumed) != 1 || len(got.Missed) != 1 || got.MissedUnread != "keploy's error drain stopped" {
		t.Fatalf("left %+v; want what was served, what was read of the misses, and why that may be short", got)
	}
}

// Nor can the CLI ask a stopped agent whether its hooks tracked every process,
// so the account carries what the kernel refused them: a compose replay fails
// on it (pkg/service/mock). "None refused" is written out, as zeros; an agent
// that cannot count leaves the field out, which the CLI reads as "unknown". A
// zero from it would vouch for a capture nobody checked.
func TestWriteStopOutcomeCarriesWhatTheHooksCouldNotTrack(t *testing.T) {
	// leave has an agent with those hooks write its outcome, and returns it
	// with what the agent logged at WARN or above.
	leave := func(t *testing.T, hooks coreAgent.Hooks) ([]byte, models.MockOutcome, []observer.LoggedEntry) {
		t.Helper()
		core, logs := observer.New(zapcore.WarnLevel)
		a := &Agent{Proxy: &outcomeProxy{consumed: []models.MockState{{Name: "mock-0"}}}, Hooks: hooks, logger: zap.New(core)}
		path := filepath.Join(t.TempDir(), "outcome.json")
		if err := a.writeStopOutcome(context.Background(), path); err != nil {
			t.Fatalf("writeStopOutcome: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("no outcome left: %v", err)
		}
		var got models.MockOutcome
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("the outcome is not a models.MockOutcome: %v\n%s", err, raw)
		}
		if len(got.Consumed) != 1 || got.Consumed[0].Name != "mock-0" {
			t.Fatalf("left %s, without the mocks this replay served", raw)
		}
		return raw, got, logs.All()
	}

	for _, want := range []models.PIDMapRefusals{
		{TargetCapacity: 10240, ExcludedCapacity: 10240}, // none refused
		{Target: 265, Excluded: 2, LastTargetTGID: 1496394, TargetCapacity: 10240, ExcludedCapacity: 10240},
	} {
		raw, got, warned := leave(t, countingHooks{refusals: want})
		if got.PIDMapRefusals == nil || *got.PIDMapRefusals != want {
			t.Fatalf("hooks that counted %+v left %s", want, raw)
		}
		if len(warned) != 0 {
			t.Fatalf("hooks that counted %+v: the agent warned %q", want, warned[0].Message)
		}
	}

	// Hooks that never count leave no count and no error: "unknown", which
	// the CLI lets pass. A count that could not be read leaves no count but
	// its error, so the CLI can tell the loss (completeness unproven, which
	// --strict fails) from an agent that never counts; and the agent says it,
	// its log being the one place left that can.
	for _, tc := range []struct {
		name    string
		hooks   coreAgent.Hooks
		wantErr string
		warns   int
	}{
		{"hooks that do not count", loadedHooks{}, "", 0},
		{"an agent without any hook", nil, "", 0},
		{"hooks whose read failed", countingHooks{err: errors.New("bad file descriptor")}, "bad file descriptor", 1},
	} {
		raw, got, warned := leave(t, tc.hooks)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, there := fields["pidMapRefusals"]; there || got.PIDMapRefusals != nil {
			t.Fatalf("%s: left %s; want no pidMapRefusals: there is no count to leave", tc.name, raw)
		}
		if got.PIDMapRefusalsError != tc.wantErr {
			t.Fatalf("%s: left pidMapRefusalsError %q, want %q: %s", tc.name, got.PIDMapRefusalsError, tc.wantErr, raw)
		}
		if len(warned) != tc.warns {
			t.Fatalf("%s: the agent logged %d line(s) at WARN or above, want %d: %+v", tc.name, len(warned), tc.warns, warned)
		}
	}
}

// settleHooks records the agent's end-of-run hook among the proxy's reads.
type settleHooks struct {
	AgentHook
	calls *[]string
	err   error
	// final reports whether the agent's test windows were final (its
	// FinalScopeWindows had closed the run) when the hook was called; it is
	// recorded as that read having come first.
	final func() bool
}

func (h *settleHooks) AfterTestRun(context.Context, string, []string, models.TestCoverage) error {
	if h.final != nil && h.final() {
		*h.calls = append(*h.calls, "FinalScopeWindows")
	}
	*h.calls = append(*h.calls, "AfterTestRun")
	return h.err
}

// settleOrderProxy is an outcomeProxy that records its reads among the hooks'.
type settleOrderProxy struct {
	outcomeProxy
	calls *[]string
}

func (p *settleOrderProxy) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	*p.calls = append(*p.calls, "GetConsumedMocks")
	return p.outcomeProxy.GetConsumedMocks(ctx)
}

// Under the proven switch an agent stopped under compose finishes its end of
// the run (its hooks' AfterTestRun) before it reads what the replay served,
// as the CLI has it do where it can reach it, and says in its account that it
// did, or why it could not: the account is left either way, and the CLI does
// not judge a run whose account lacks the word. Without the switch the hook is
// not called and the account is encoded as before.
func TestWriteStopOutcomeSettlesTheRunUnderTheSwitch(t *testing.T) {
	for _, c := range []struct {
		name      string
		proven    bool
		hookErr   error
		wantCalls []string
		want      models.MockOutcome
	}{
		{"off", false, nil, []string{"GetConsumedMocks"}, models.MockOutcome{}},
		{"settled", true, nil, []string{"FinalScopeWindows", "AfterTestRun", "GetConsumedMocks"}, models.MockOutcome{Settled: true}},
		{"hook failed", true, errors.New("keploy could not write the row of a program off the run's clock"), []string{"FinalScopeWindows", "AfterTestRun", "GetConsumedMocks"},
			models.MockOutcome{SettleError: "keploy could not write the row of a program off the run's clock"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sw := ""
			if c.proven {
				sw = "1"
			}
			t.Setenv(provenswitch.Env, sw)
			var calls []string
			prev := ActiveHooks
			a := &Agent{Proxy: &settleOrderProxy{calls: &calls}, logger: zap.NewNop()}
			ActiveHooks = &settleHooks{calls: &calls, err: c.hookErr, final: a.wireClosed}
			t.Cleanup(func() { ActiveHooks = prev })
			path := filepath.Join(t.TempDir(), "outcome.json")
			if err := a.writeStopOutcome(context.Background(), path); err != nil {
				t.Fatalf("writeStopOutcome: %v", err)
			}
			if !reflect.DeepEqual(calls, c.wantCalls) {
				t.Fatalf("calls %v, want %v", calls, c.wantCalls)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("no outcome left: %v", err)
			}
			var got models.MockOutcome
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("left %+v, want %+v", got, c.want)
			}
			if !c.proven && string(raw) != `{"consumed":null,"missed":null}` {
				t.Fatalf("the account without the switch is %s, want it encoded as before", raw)
			}
		})
	}
}
