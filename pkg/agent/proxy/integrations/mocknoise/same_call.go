package mocknoise

import (
	"strings"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

// BothWaysDiffer is an Adapter that compares a recorded and a live request
// body both ways and whole (SameCallDrift): drift, as Diff's ("body."-
// prefixed, the whole body "body"), is every field at which they are not
// the same body outside known noise, a field only the live body holds
// among them, and comparable false where neither has fields to compare.
// Like Diff, it reads known and typeStrict only.
type BothWaysDiffer interface {
	DiffBothWays(m *models.Mock, recorded, live *JSONBody, known map[string][]string, typeStrict map[string]struct{}, valIsNoise func(string) bool) (drift map[string][]string, comparable bool)
}

// SameCallDrift is the drift at which live, a call's request body, is not
// recorded, m's recorded body (ok false: m has none, and nothing drifts),
// outside the noise keploy.yml declares (userNoise) and the noise the record
// learned on m, read as StrictRejectRecordedJSONBody reads it under strict
// enforcement, whichever noise mode the replay runs in, but compared both
// ways (BothWaysDiffer): a field only the live body holds is drift too.
// Bodies with no fields to compare drift whole ("body") unless they are the
// same bytes. nil: m is a recording of this very call's body.
func SameCallDrift(a interface {
	Adapter
	BothWaysDiffer
}, m *models.Mock, recorded *JSONBody, ok bool, live *JSONBody, userNoise map[string][]string) map[string][]string {
	if !ok {
		return nil
	}
	learned := userDecided(userNoise, StripBodyPrefix(a.StoredNoise(m)))
	valIsNoise := a.RecordedValueIsNoise(m)
	d, comparable := a.DiffBothWays(m, recorded, live, knownWithLearned(userNoise, learned), learnedTypeStrict(userNoise, learned), valIsNoise)
	if !comparable {
		if string(recorded.Raw()) == string(live.Raw()) {
			return nil
		}
		return map[string][]string{"body": nil}
	}
	if len(learned) > 0 && hasPatterns(userNoise) {
		// As userGuardedDrift: keploy.yml's entries with patterns decide
		// the fields they guard before any learned noise.
		ud, _ := a.DiffBothWays(m, recorded, live, userNoise, nil, valIsNoise)
		user := matcher.NewNoiseLookup(userNoise)
		for p, v := range ud {
			if pats, ok := user.Patterns(strings.TrimPrefix(p, "body.")); ok && len(pats) > 0 {
				if d == nil {
					d = map[string][]string{}
				}
				d[p] = v
			}
		}
	}
	if len(d) == 0 {
		return nil
	}
	return d
}

// Decoded is the body as json.Unmarshal decodes it into an interface{} (ok
// false: it does not decode, so it is not JSON as pkg.IsJSON reads it),
// decoded once.
func (b *JSONBody) Decoded() (value any, ok bool) {
	return b.decodedValue()
}

// NewReadJSONBody is raw as a JSONBody already validated and decoded: one
// that many goroutines may read at once, as a body kept for many calls is,
// since no read of it changes it.
func NewReadJSONBody(raw []byte) *JSONBody {
	b := &JSONBody{raw: raw}
	b.isJSON()
	b.decodedValue()
	return b
}
