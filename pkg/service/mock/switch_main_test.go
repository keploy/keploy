package mock

import (
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch/switchtest"
)

// The rows here check the rules with the proven switch off unless a row turns
// it on: a KEPLOY_PROVEN this test process inherited chooses nothing.
func TestMain(m *testing.M) { switchtest.Main(m) }

func TestARowStartsWithTheProvenSwitchOff(t *testing.T) { switchtest.OffAtEveryRowsStart(t) }
