package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
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
// have answered, in the one document the CLI decodes.
func TestWriteStopOutcomeLeavesWhatTheReplayServedAndMissed(t *testing.T) {
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
	want := models.MockOutcome{Consumed: proxy.consumed, Missed: proxy.missed}
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
