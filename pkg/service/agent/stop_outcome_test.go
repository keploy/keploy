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
}

func (p *outcomeProxy) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	if p.block != nil {
		<-p.block
	}
	return p.consumed, p.consumedErr
}

func (p *outcomeProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
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
func TestLeaveStopOutcomeGivesUpOnAWedgedProxy(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	a := &Agent{Proxy: &outcomeProxy{block: block}, logger: zap.NewNop()}
	path := filepath.Join(t.TempDir(), "outcome.json")

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
}
