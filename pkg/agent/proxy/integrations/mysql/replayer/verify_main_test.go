package replayer_test

import (
	"os"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
)

// The exact view's verify mode is on in every row: each read the real
// manager's view keeps materialised (its keyed walks, the per-test tier read
// a few at a time) is recomputed the way the view computed it before it kept
// it, and any difference panics. A row that counts the keys or reads a call
// costs turns it off (withoutViewVerify), since the recomputation is the cost
// it counts.
//
// So is the check of the recorded bodies the strict gate makes decoded
// (replayer.CheckDecodedBodies): each recording every row compares is checked,
// once per reading, against json.Unmarshal of its encoded body.
func TestMain(m *testing.M) {
	integrations.ExactViewVerify.Store(true)
	replayer.CheckDecodedBodies()
	os.Exit(m.Run())
}

// withoutViewVerify turns the exact view's verify mode off for the rest of t.
// The switch is package-wide, so a row that turns it off runs alone: it fails
// when called while a large row runs beside the others (sideBySide).
func withoutViewVerify(t testing.TB) {
	t.Helper()
	if n := largeRowsRunning.Load(); n > 0 {
		t.Fatalf("the verify mode is package-wide: turned off while %d large rows run side by side, it would be off in theirs too", n)
	}
	was := integrations.ExactViewVerify.Load()
	integrations.ExactViewVerify.Store(false)
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
}

// largeRowsRunning counts the large rows running side by side (sideBySide).
var largeRowsRunning atomic.Int64

// sideBySide runs the large row t beside the package's other large rows
// (t.Parallel), so under load the package holds as many runnable threads as
// go test allows (-parallel, GOMAXPROCS) instead of one. Go starts a top-level
// parallel test only once every sequential one has ended, so the rows that
// count package-wide calls (CountStatementKeysForTest, ConfigTagReadsForTest,
// CountUnnamedExecKeysForTest) or turn the verify mode off
// (withoutViewVerify), all sequential, never run beside it; withoutViewVerify
// fails if one does. A row that calls sideBySide builds its own sets, stores,
// decode contexts and noise engines, and writes no package-level variable.
func sideBySide(t *testing.T) {
	t.Helper()
	t.Parallel()
	largeRowsRunning.Add(1)
	t.Cleanup(func() { largeRowsRunning.Add(-1) })
}
