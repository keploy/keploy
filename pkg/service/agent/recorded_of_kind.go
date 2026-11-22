package agent

import (
	coreAgent "go.keploy.io/server/v3/pkg/agent"
)

// readKindFromDisk points the proxy's reads of the staged set's recordings of
// a kind (SetRecordedOfKind) at the on-disk store at every staging, under
// either switch: a test's own are on disk until it opens, and a parser that
// holds a call until a later window serves it holds it only while the set
// still holds an unserved recording of it
// (integrations.UnservedRecordingFinder), those on disk too. A set kept all
// resident sets none: its trees hold every recording. The setter fails only
// for a proxy with no mock manager, which has no trees to stage nor a parser
// to hold a call: there is nothing to read for.
func (a *Agent) readKindFromDisk() {
	if rk, ok := a.Proxy.(coreAgent.RecordedOfKindSetter); ok {
		_ = rk.SetRecordedOfKind(a.diskRecordedOfKind())
	}
}
