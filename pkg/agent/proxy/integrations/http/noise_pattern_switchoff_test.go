package http

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// With the proven switch off and strict enforcement on, a recorded value the
// obfuscator redacted under a keploy.yml request-body pattern holds the live
// value to the pattern, as the switch's strict comparison does: in the
// noise-aware exact body match (a whole one-key form the obfuscator's key
// pattern matches, a form, a JSON body) and in mock serving's body check
// (mocknoise: the form differ and the JSON diff). A live value outside the
// pattern, or none, is not served; one inside is. Before, the redacted
// recorded value excused the field whatever the live value.
func TestWithTheSwitchOffARedactedRecordingHoldsTheLiveValueToKeployYmlPatterns(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	ct := map[string]string{"Content-Type": "application/json"}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	tok := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}})
	// The obfuscator's noise: a pattern for each value it redacts and, for a
	// form value, one for the key's segment.
	jsonNoise := []string{`^REDACTED_abc$`}
	byKey := []string{`^REDACTED_abc$`, `^token=[^&]+$`}
	for _, c := range []struct {
		name      string
		hdr       map[string]string
		rec, live string
		noise     []string
		user      map[string][]string
		served    bool
	}{
		{"a JSON field, the live value inside", ct, `{"token":"REDACTED_abc","n":1}`, `{"token":"t-1","n":1}`, jsonNoise, tok.Body, true},
		{"a JSON field, the live value outside", ct, `{"token":"REDACTED_abc","n":1}`, `{"token":"evil","n":1}`, jsonNoise, tok.Body, false},
		{"a JSON field the live body lacks", ct, `{"token":"REDACTED_abc","n":1}`, `{"n":1}`, jsonNoise, tok.Body, false},
		{"a JSON field no pattern guards", ct, `{"token":"REDACTED_abc","n":1}`, `{"token":"evil","n":1}`, jsonNoise, nil, true},
		{"a one-key form, the live value inside", form, `token=REDACTED_abc`, `token=t-1`, byKey, tok.Body, true},
		{"a one-key form, the live value outside", form, `token=REDACTED_abc`, `token=evil`, byKey, tok.Body, false},
		{"a one-key form no pattern guards", form, `token=REDACTED_abc`, `token=evil`, byKey, nil, true},
		{"a form key, the live value inside", form, `token=REDACTED_abc&x=a`, `token=t-1&x=a`, byKey, tok.Body, true},
		{"a form key, the live value outside", form, `token=REDACTED_abc&x=a`, `token=evil&x=a`, byKey, tok.Body, false},
		{"an escaped redacted form value, the live value inside", form, `token=REDACTED%2Fabc%3D&x=a`, `token=t-1&x=a`, []string{`^REDACTED/abc=$`, `^token=[^&]+$`}, tok.Body, true},
		{"a form key the live body lacks", form, `token=REDACTED_abc&x=a`, `x=a`, byKey, tok.Body, false},
		{"a form key no pattern guards", form, `token=REDACTED_abc&x=a`, `token=evil&x=a`, byKey, nil, true},
		{"a repeated form key, a recorded value inside", form, `token=REDACTED_abc&token=t-3`, `token=t-1&token=t-2`, byKey, tok.Body, true},
		{"a repeated form key, a recorded value outside", form, `token=REDACTED_abc&token=bbbbbbb`, `token=t-1&token=t-2`, byKey, tok.Body, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := recorded("m", "/x", c.hdr, c.rec)
			m.Noise = c.noise
			db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
			ok, _, _, err := newHTTP().match(context.Background(), sent("/x", c.hdr, c.live), db, nil, c.user, nil, true, false, true, true, true)
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.served {
				t.Errorf("served %v; want %v", ok, c.served)
			}
			allowed, drift := mocknoise.New(httpNoiseAdapter{}, true, true).StrictReject(m, []byte(c.live), c.user)
			if allowed != c.served {
				t.Errorf("mock serving's body check allowed %v (drift %v); want %v", allowed, drift, c.served)
			}
		})
	}
}

// A body the obfuscator redacted whole is still served with the switch off,
// a keploy.yml pattern on a field or not: it has no field the pattern names.
func TestWithTheSwitchOffAWholeRedactedBodyIsServed(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	tok := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}})
	m := recorded("m", "/x", form, `REDACTED_abc`)
	m.Noise = []string{`^REDACTED_abc$`}
	db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
	ok, _, _, err := newHTTP().match(context.Background(), sent("/x", form, `s3cr3t-tok`), db, nil, tok.Body, nil, true, false, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a body the obfuscator redacted whole was not served")
	}
}

// With the switch off, a form field keploy.yml's request-body pattern guards
// is held to it under strict enforcement, a value the obfuscator redacted or
// not: a form is not value-checked elsewhere there, so before, any form was
// served by the fuzzy match. Without strict enforcement the patterns decide
// nothing, as before.
func TestWithTheSwitchOffAFormFieldOutsideKeployYmlPatterns(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	tok := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}})
	for _, strict := range []bool{true, false} {
		m := recorded("m", "/x", form, `token=t-1&x=a`)
		db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
		ok, _, diag, err := newHTTP().match(context.Background(), sent("/x", form, `token=evil&x=a`), db, nil, tok.Body, nil, true, false, strict, true, true)
		if err != nil {
			t.Fatal(err)
		}
		if ok == strict {
			t.Fatalf("strict enforcement %v: served %v", strict, ok)
		}
		if strict && (diag == nil || diag.phase != models.MatchPhaseStrict) {
			t.Fatalf("the miss is %+v; want the strict phase", diag)
		}
	}
}

// With the switch off and strict enforcement on, the fuzzy match of a form
// picks among the recordings keploy.yml's patterns hold: the closest one,
// whose guarded field is outside them, is not served while another is.
func TestWithTheSwitchOffTheFuzzyMatchPicksAmongTheRecordingsPatternsHold(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	tok := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}})
	outside := recorded("outside", "/x", form, `token=evim&x=a`)
	held := recorded("held", "/x", form, `token=evil&x=bbbbbbbb`)
	db := &mockMemDb{mocks: []*models.Mock{outside, held}, updateUnFilteredReturn: true}
	ok, got, _, err := newHTTP().match(context.Background(), sent("/x", form, `token=evil&x=a`), db, nil, tok.Body, nil, true, false, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got == nil || got.Name != "held" {
		t.Fatalf("served %v %v; want the recording the patterns hold", ok, got)
	}
}
