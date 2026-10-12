package replay

import (
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// strictFailure is the veto of the OBSOLETE demotion: a test whose response
// failed is FAILED even when its consumed mock set also diverged from the
// recording. Opt-in through test.strictFailure (--strict-failure), and always
// on under the proven switch, where a response that differs fails its test
// whatever the mocks did.
func (r *Replayer) strictFailure() bool {
	return r.config.Test.StrictFailure || provenswitch.On()
}

// additiveAutoPassOff reports whether a response that only gained fields is
// failed like any other difference rather than passed by default: under
// auto-replay, which replays the very build it recorded, and under the proven
// switch, which asserts every response field.
func (r *Replayer) additiveAutoPassOff() bool {
	return r.config.Test.AutoReplay || provenswitch.On()
}

// responseIgnoreOrdering is the ignoreOrdering the response comparison runs
// with (matcherUtils.ResponseIgnoreOrdering): off under the proven switch. The
// replay's template layer reads it too, so what it asserts of a variable's
// place in an array, and what it refuses there, follow the comparison the
// test case gets. (A streamed use is asserted at its index either way:
// templates.Run.StreamUseFailures.) keploy templatize keeps the configured
// value on purpose: what it writes must replay with the switch off too, and a
// capture it wrote inside an array under the switch would be refused then.
func (r *Replayer) responseIgnoreOrdering() bool {
	return matcherUtils.ResponseIgnoreOrdering(r.config.Test.IgnoreOrdering)
}
