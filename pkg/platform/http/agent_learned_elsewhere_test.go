package http_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
)

// learnedProxy is an agent's proxy whose learn step taught recordings from
// calls made in other windows than theirs (coreAgent.LearnedElsewhereReader).
type learnedProxy struct {
	coreAgent.Proxy
	learned []models.MockState
}

func (p *learnedProxy) LearnedElsewhere() []models.MockState { return p.learned }

// learnedSvc answers the route's read with the agent service's own
// (agentsvc.Agent.LearnedElsewhere) over its proxy.
type learnedSvc struct {
	stubSvc
	agent *agentsvc.Agent
}

func (s *learnedSvc) LearnedElsewhere(ctx context.Context) ([]models.MockState, error) {
	return s.agent.LearnedElsewhere(ctx)
}

// What record's full round learned onto recordings from calls made in other
// windows than theirs reaches keploy mock record from a native agent only
// through the agent's read and its route (/mock/learned-elsewhere): the
// agent service asks its proxy, the route answers it, and the CLI's client
// reads it. An agent that cannot say answers so (501) and is read as one
// that learned none; one that learned none, or whose proxy cannot say,
// answers an empty list.
func TestWhatTheAgentLearnedAcrossWindowsReachesTheCLI(t *testing.T) {
	learned := []models.MockState{{
		Name: "mock-9",
		Clocks: map[string]models.ClockOffset{"claimed_at": {
			Min: -time.Minute, Max: time.Minute, Res: time.Millisecond, Zone: "+05:30", Layout: "rfc3339", AppZone: "Asia/Kolkata",
			Since: models.SinceTestBegin, Word: models.ClockWordOf("stop-1793336961102639022", len("stop-"), len("stop-1793336961102639022")),
			Why: "claimed_at reads the clock", Step: time.Hour,
		}},
		TaughtIn: []models.TeachingWindow{{Test: "test-2", At: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}, {At: time.Date(2026, 11, 1, 9, 0, 2, 0, time.UTC)}},
	}}
	for _, c := range []struct {
		name string
		svc  agentsvc.Service
		want []models.MockState
	}{
		{"an agent whose proxy learned", &learnedSvc{agent: &agentsvc.Agent{Proxy: &learnedProxy{learned: learned}}}, learned},
		{"an agent whose proxy learned none", &learnedSvc{agent: &agentsvc.Agent{Proxy: &learnedProxy{}}}, []models.MockState{}},
		{"an agent whose proxy cannot say", &learnedSvc{agent: &agentsvc.Agent{Proxy: struct{ coreAgent.Proxy }{}}}, []models.MockState{}},
		{"an agent that cannot say", &stubSvc{}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, uri := serveHistory(t, c.svc)
			client := newClient(t, uri)
			got, err := client.GetLearnedElsewhere(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("the CLI read %#v as learned across windows; want %#v", got, c.want)
			}
		})
	}
}
