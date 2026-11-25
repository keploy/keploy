package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	agenthttp "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// unsetBackgroundSvc is onceSvc that also takes the set's background
// recordings' names (scopeBackgroundSetter): what the route installs with
// the switch off for a set that has some.
type unsetBackgroundSvc struct {
	onceSvc
	background *[]string
}

func (s unsetBackgroundSvc) SetScopeBackground(_ context.Context, names []string) error {
	*s.background = names
	s.order = append(s.order, "background")
	return nil
}

// With the switch off, keploy mock replay's table for a set with background
// recordings (pushScopeTable: PushScopeTableWithBackground) carries the
// worker scope's series and the ones that answer once in a run, as a
// startup: true rule's createIndexes moved from startup: to worker: is, and
// the route installs them with the worker scope, before the pool, whose
// serving hands them to the proxy: the background a test's window is served
// answers a once series' recording one call in the run, at the
// application's boot too. Left out, the proxy reads that recording as one
// the windows read apart from the calls outside every test, and answers it
// again in a test after the boot answered it.
func TestWithTheSwitchOffABackgroundTableInstallsItsOnceSeriesBeforeThePool(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	ps, series, once, background := &poolSvc{}, map[string][]string{}, []string(nil), []string(nil)
	svc := unsetBackgroundSvc{onceSvc{seriesSvc{workerSvc{ps}, &series, nil}, &once, nil}, &background}
	srv := httptest.NewServer(http.HandlerFunc((&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable))
	t.Cleanup(srv.Close)
	client := agenthttp.New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
	const id = "startup-indexes/-/1"
	if err := client.PushScopeTableWithBackground(context.Background(),
		map[string][]string{".::TestAppCRUD": {"mock-0"}}, nil,
		map[string][]string{models.PoolKeyNone: {"mock-9"}}, map[string][]string{models.PoolKeyNone: {"mock-9"}},
		[]string{"mock-9"}, map[string][]string{id: {"mock-9"}}, []string{id}, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(series[id], []string{"mock-9"}) || !slices.Equal(once, []string{id}) {
		t.Fatalf("the agent installed series %v and once series %v; want the table's", series, once)
	}
	if !slices.Equal(ps.order, []string{"worker", "series", "once", "background", "pool"}) {
		t.Fatalf("the agent installed %v; want the series and once series with the worker scope, before the pool", ps.order)
	}
}
