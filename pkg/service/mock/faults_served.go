package mock

import (
	"sort"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// faultMocks is the mocks of pools whose answer ends in a fault, by name: what
// the replay summary names when the agent served one (logFaultsServed).
func faultMocks(pools ...[]*models.Mock) map[string]*models.Mock {
	out := map[string]*models.Mock{}
	for _, pool := range pools {
		for _, mk := range pool {
			if mk != nil && mk.Spec.Fault != nil {
				out[mk.Name] = mk
			}
		}
	}
	return out
}

// logFaultsServed names each fault mock the agent served, one line each in
// the order it first served them ("mock-12: upstream reset after 0 bytes, as
// recorded"): the application's error handling met that fault, as the upstream
// gave it at record, not an answer. A fault mock no call consumed is not
// named: it was not served.
func logFaultsServed(logger *zap.Logger, consumed []models.MockState, faults map[string]*models.Mock) {
	served := make([]models.MockState, 0, len(faults))
	for _, state := range consumed {
		if _, ok := faults[state.Name]; ok {
			served = append(served, state)
		}
	}
	sort.SliceStable(served, func(i, j int) bool { return served[i].Timestamp < served[j].Timestamp })
	for _, state := range served {
		logger.Info("served a mock's fault", zap.String("fault", faults[state.Name].FaultServed()))
	}
}
