package switchtest

import "testing"

func TestMain(m *testing.M) { Main(m) }

// Run with KEPLOY_PROVEN=1 in its environment, this package's rows start with
// the switch off.
func TestARowStartsWithTheSwitchOff(t *testing.T) { OffAtEveryRowsStart(t) }
