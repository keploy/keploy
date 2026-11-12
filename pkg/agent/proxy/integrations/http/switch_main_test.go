package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/provenswitch/switchtest"
)

// The rows here check the rules with the proven switch off unless a row turns
// it on: a KEPLOY_PROVEN this test process inherited chooses nothing.
// The exact view's verify mode is on in every row: each recording the learn
// step reads as it kept it parsed (recordedParts) is parsed again and
// compared, so a row that changes a recording in place panics.
func TestMain(m *testing.M) {
	integrations.ExactViewVerify.Store(true)
	switchtest.Main(m)
}

func TestARowStartsWithTheProvenSwitchOff(t *testing.T) { switchtest.OffAtEveryRowsStart(t) }
