package integrations

import (
	"os"
	"sync/atomic"
)

// ExactViewVerify is the exact view's verify mode, for the proxy and its
// parsers alike: each read kept instead of computed on every call (an index
// the view built once, a recording a parser parsed once) is recomputed the
// way it was before it was kept, and any difference panics. On in the
// proxy's and the parsers' rows and wherever KEPLOY_EXACT_VIEW_VERIFY=1
// (CI's lanes under the proven switch; the agent takes it from its
// --exact-view-verify flag, pkg/agent/proxy.SetExactViewVerify); off in a
// user's run, where the recomputation is the cost the kept read saves.
var ExactViewVerify atomic.Bool

func init() { ExactViewVerify.Store(os.Getenv("KEPLOY_EXACT_VIEW_VERIFY") == "1") }
