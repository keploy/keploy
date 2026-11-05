package agent

import (
	"context"
	"slices"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// SetRunPlaceWindows installs, for keploy mock replay with the proven switch
// off, the recorded window of every test of the set it serves
// (models.ScopeTableReq.Windows), for the run's place alone
// (coreAgent.RunPlaceSeeder): an answer recorded in a later window than its
// call is written once the replay reaches that window. Nothing else reads
// them: a parser, the learn step and the window release rules behave as
// before. None leaves the set none, and its answers are written at once.
func (a *Agent) SetRunPlaceWindows(_ context.Context, ws []models.TestWindow) error {
	if s, ok := a.Proxy.(coreAgent.RunPlaceSeeder); ok {
		s.SeedRunPlaceWindows(slices.Clone(ws))
	}
	return nil
}

// reachRunTest moves the run's place at a replayed test's begin (open) or
// end. Under the proven switch the test's exact view moves it too (the
// proxy's serveExactTest and ServeExactPool): the place never goes back, so
// a second move to the same anchor is none. A test the run's windows do not
// name, and a record, move nothing.
func (a *Agent) reachRunTest(name string, open bool) {
	if s, ok := a.Proxy.(coreAgent.RunPlaceSeeder); ok {
		s.ReachRunTest(name, open)
	}
}
