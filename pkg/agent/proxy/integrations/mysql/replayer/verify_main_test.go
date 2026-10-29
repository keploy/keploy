package replayer_test

import (
	"os"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
)

// The exact view's verify mode is on in every row: each read the real
// manager's view keeps materialised (its keyed walks, the per-test tier read
// a few at a time) is recomputed the way the view computed it before it kept
// it, and any difference panics. A row that counts the keys or reads a call
// costs turns it off (withoutViewVerify), since the recomputation is the cost
// it counts.
func TestMain(m *testing.M) {
	integrations.ExactViewVerify.Store(true)
	os.Exit(m.Run())
}

// withoutViewVerify turns the exact view's verify mode off for the rest of t.
func withoutViewVerify(t testing.TB) {
	was := integrations.ExactViewVerify.Load()
	integrations.ExactViewVerify.Store(false)
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
}
