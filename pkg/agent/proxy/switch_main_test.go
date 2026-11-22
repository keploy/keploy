package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch/switchtest"
)

// The rows here check the rules with the proven switch off unless a row turns
// it on: a KEPLOY_PROVEN this test process inherited chooses nothing. Every
// row runs with the exact view's verify mode on (exactViewVerify): a read the
// view keeps materialised that differs from the same read recomputed fails
// the row.
func TestMain(m *testing.M) {
	exactViewVerify.Store(true)
	switchtest.Main(m)
}

func TestARowStartsWithTheProvenSwitchOff(t *testing.T) { switchtest.OffAtEveryRowsStart(t) }
