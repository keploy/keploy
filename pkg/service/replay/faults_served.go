package replay

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// logFaultsServed names each fault mock the set's replay served, one line
// each in the order it first served them ("mock-12: upstream reset after 0
// bytes, as recorded"): the application's error handling met that fault, as
// the upstream gave it at record, not an answer. A fault mock no call
// consumed is not named: it was not served.
func logFaultsServed(logger *zap.Logger, testSetID string, consumed map[string]models.MockState, lookup map[string]mockDisplayInfo) {
	states := make([]models.MockState, 0, len(consumed))
	for _, st := range consumed {
		states = append(states, st)
	}
	for _, line := range models.FaultsServed(states, func(name string) string { return lookup[name].fault }) {
		logger.Info("served a mock's fault", zap.String("testSet", testSetID), zap.String("fault", line))
	}
}
