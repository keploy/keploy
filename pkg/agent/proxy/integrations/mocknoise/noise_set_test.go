package mocknoise

import (
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// strictRejectAsBefore is StrictRejectRecordedJSONBody as it was before it
// compared a recording with no learned noise under the caller's own noise:
// a merged copy (KnownNoise) and a type-strict set for every recording. The
// row below holds the engine to its verdicts.
func strictRejectAsBefore(e *Engine, m *models.Mock, recorded *JSONBody, ok bool, live *JSONBody, userNoise map[string][]string) (bool, map[string][]string) {
	stored := e.adapter.StoredNoise(m)
	if !e.strict && len(stored) == 0 {
		return true, nil
	}
	if !ok {
		return true, nil
	}
	learned := StripBodyPrefix(e.adapter.StoredNoise(m))
	userKeys := make(map[string]struct{}, len(userNoise))
	for k := range userNoise {
		userKeys[strings.ToLower(k)] = struct{}{}
	}
	typeStrict := make(map[string]struct{}, len(learned))
	for k := range learned {
		if _, userConfigured := userKeys[strings.ToLower(k)]; userConfigured {
			continue
		}
		typeStrict[k] = struct{}{}
	}
	var d map[string][]string
	var comparable bool
	if jd, isJSON := e.adapter.(JSONBodyDiffer); isJSON {
		d, comparable = jd.DiffJSONBodies(m, recorded, live, e.KnownNoise(m, userNoise), typeStrict, false, e.adapter.RecordedValueIsNoise(m))
	} else {
		d, comparable = e.adapter.Diff(m, recorded.Raw(), live.Raw(), e.KnownNoise(m, userNoise), typeStrict, false, e.adapter.RecordedValueIsNoise(m))
	}
	if !comparable {
		if string(recorded.Raw()) == string(live.Raw()) {
			return true, nil
		}
		return false, map[string][]string{"body": nil}
	}
	if len(d) == 0 {
		return true, nil
	}
	return false, d
}

// knownSpy is fakeAdapter recording the known set its Diff was given last.
type knownSpy struct {
	fakeAdapter
	known map[string][]string
}

func (s *knownSpy) Diff(m *models.Mock, recorded, live []byte, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, valIsNoise func(string) bool) (map[string][]string, bool) {
	s.known = known
	return s.fakeAdapter.Diff(m, recorded, live, known, typeStrict, valueChangesOnly, valIsNoise)
}

// knownBodySpy is knownSpy comparing through DiffJSONBodies, as MySQL's
// adapter does.
type knownBodySpy struct{ *knownSpy }

func (s knownBodySpy) DiffJSONBodies(_ *models.Mock, recorded, live *JSONBody, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, valIsNoise func(string) bool) (map[string][]string, bool) {
	s.known = known
	return DetectJSONBodyDrift(recorded, live, known, typeStrict, valueChangesOnly, valIsNoise)
}

func mapPointer(m map[string][]string) uintptr { return reflect.ValueOf(m).Pointer() }

// TestARecordingWithNoLearnedNoiseIsComparedUnderTheCallersOwnNoise: strict
// enforcement gives a recording with no learned noise the caller's noise
// itself (no copy a recording), and one with learned noise a merged copy,
// never writing to the caller's noise; and every verdict and drift, through
// Diff and DiffJSONBodies, strict or not, with and without the proven switch
// (which drops learned noise outside the body), is the one the copy got
// before (strictRejectAsBefore).
func TestARecordingWithNoLearnedNoiseIsComparedUnderTheCallersOwnNoise(t *testing.T) {
	const recorded = `{"id":1,"Name":"ab","n":2,"tags":["x"],"parameters":[{"type":3,"value":"1"}]}`
	lives := []string{
		recorded,
		`{"id":2,"Name":"ab","n":2,"tags":["x"],"parameters":[{"type":3,"value":"1"}]}`,
		`{"id":"2","Name":"ab","n":2,"tags":["x"],"parameters":[{"type":3,"value":"1"}]}`,
		`{"id":1,"Name":"zz","n":3,"tags":["y"],"parameters":[{"type":8,"value":"2"}]}`,
		`{"id":1,"Name":"ab","tags":["x"],"parameters":[{"type":3,"value":"1"}]}`,
		`not json`,
	}
	users := []map[string][]string{
		nil,
		{},
		{"id": {}},
		{"name": {"^z+$"}},
		{"parameters.0.type": {}, "n": {}},
	}
	storeds := []map[string][]string{
		nil,
		{},
		{"body.id": {}},
		{"body.Name": {}, "body.n": {}},
		{"ID": {}},
		{"header.x-trace": {}, "url.query.at": {}},
		{"body.tags": {}, "header.x-trace": {}},
	}
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		for _, strict := range []bool{false, true} {
			for _, viaBodies := range []bool{false, true} {
				for si, stored := range storeds {
					for ui, user := range users {
						for li, live := range lives {
							spy := &knownSpy{fakeAdapter: fakeAdapter{body: []byte(recorded), hasBody: true, stored: stored}}
							var a Adapter = spy
							if viaBodies {
								a = knownBodySpy{spy}
							}
							e := New(a, false, strict)
							userBefore, storedBefore := maps.Clone(user), maps.Clone(stored)
							wantOK, wantDrift := strictRejectAsBefore(e, nil, NewJSONBody([]byte(recorded)), true, NewJSONBody([]byte(live)), user)
							spy.known = nil
							gotOK, gotDrift := e.StrictRejectRecordedJSONBody(nil, NewJSONBody([]byte(recorded)), true, NewJSONBody([]byte(live)), user)
							name := func() string {
								return fmt.Sprintf("proven=%q strict=%v bodies=%v stored#%d user#%d live#%d", proven, strict, viaBodies, si, ui, li)
							}
							if gotOK != wantOK || !reflect.DeepEqual(gotDrift, wantDrift) {
								t.Errorf("%s: got (%v, %v), before (%v, %v)", name(), gotOK, gotDrift, wantOK, wantDrift)
							}
							if !reflect.DeepEqual(user, userBefore) || !reflect.DeepEqual(stored, storedBefore) {
								t.Errorf("%s: the compare wrote to its noise: user %v (was %v), stored %v (was %v)", name(), user, userBefore, stored, storedBefore)
							}
							if !strict && len(stored) == 0 {
								continue // not compared
							}
							if len(StripBodyPrefix(stored)) == 0 {
								if mapPointer(spy.known) != mapPointer(user) {
									t.Errorf("%s: nothing learned, yet the compare got a noise set of its own, not the caller's", name())
								}
							} else if user != nil && mapPointer(spy.known) == mapPointer(user) {
								t.Errorf("%s: learned noise, yet the compare got the caller's own noise", name())
							}
						}
					}
				}
			}
		}
	}
}
