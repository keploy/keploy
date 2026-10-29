package http

import (
	"os"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/identity"
	"go.keploy.io/server/v3/pkg/provenswitch/switchtest"
)

// The rows here check the rules with the proven switch off unless a row turns
// it on: a KEPLOY_PROVEN this test process inherited chooses nothing. The
// replay pod's own name (identity.PodNameEnv) is fixed before any row reads
// it: without it the name is the test machine's hostname, which a CI
// container may name localhost or too short to alias.
func TestMain(m *testing.M) {
	_ = os.Setenv(identity.PodNameEnv, "keploy-test-live-pod")
	switchtest.Main(m)
}

func TestARowStartsWithTheProvenSwitchOff(t *testing.T) { switchtest.OffAtEveryRowsStart(t) }
