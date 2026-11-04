package http

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/matcher"
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

// With the switch off and strict enforcement on, keploy.yml's request-body
// entry with patterns decides a form key it names as the switch's comparison
// reads it: on the key decoded, without case, a field name at any depth. A
// key with an uppercase letter (an AWS query body's RoleSessionName, the
// IRSA WebIdentityToken), a dotted key under a field-name entry and an
// escaped key are each served while both values are inside the patterns and
// refused when the live one is outside, a recorded value the obfuscator
// redacted or not, with the switch on and off. Before, the switch-off form
// reader found an entry by its exact key: such a key both inside was refused,
// and a redacted one outside was served.
func TestAFormKeyIsHeldToTheEntryThatNamesItWithTheSwitchOnAndOff(t *testing.T) {
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	role := requestBodyNoise(map[string][]string{"RoleSessionName": {`^s-\d+$`}}).Body
	tok := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}}).Body
	idEntry := requestBodyNoise(map[string][]string{"user[id]": {`^\d+$`}}).Body
	web := requestBodyNoise(map[string][]string{"WebIdentityToken": {`^eyJ`}}).Body
	webNoise := []string{`^REDACTED_abc$`, `^WebIdentityToken=[^&]+$`}
	for _, c := range []struct {
		name      string
		user      map[string][]string
		rec, live string
		noise     []string
		served    bool
	}{
		{"a CamelCase key, both inside", role, `Action=AssumeRole&RoleSessionName=s-1`, `Action=AssumeRole&RoleSessionName=s-2`, nil, true},
		{"a CamelCase key, the live value outside", role, `Action=AssumeRole&RoleSessionName=s-1`, `Action=AssumeRole&RoleSessionName=x`, nil, false},
		{"a dotted key a field-name entry names, both inside", tok, `a.token=t-1&x=a`, `a.token=t-2&x=a`, nil, true},
		{"a dotted key a field-name entry names, the live value outside", tok, `a.token=t-1&x=a`, `a.token=evil&x=a`, nil, false},
		{"an escaped key, both inside", idEntry, `user%5Bid%5D=1&x=a`, `user%5Bid%5D=2&x=a`, nil, true},
		{"an escaped key, the live value outside", idEntry, `user%5Bid%5D=1&x=a`, `user%5Bid%5D=evil&x=a`, nil, false},
		{"a redacted CamelCase key, the live value inside", web, `Action=AssumeRole&WebIdentityToken=REDACTED_abc`, `Action=AssumeRole&WebIdentityToken=eyJhbGc`, webNoise, true},
		{"a redacted CamelCase key, the live value outside", web, `Action=AssumeRole&WebIdentityToken=REDACTED_abc`, `Action=AssumeRole&WebIdentityToken=evil`, webNoise, false},
		{"a repeated key redacted second, the others inside", tok, `token=t-3&token=REDACTED_abc`, `token=t-1&token=t-2`, []string{`^REDACTED_abc$`, `^token=[^&]+$`}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Run("switch off", func(t *testing.T) {
				t.Setenv(provenswitch.Env, "")
				m := recorded("m", "/x", form, c.rec)
				m.Noise = c.noise
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
				ok, _, diag, err := newHTTP().match(context.Background(), sent("/x", form, c.live), db, nil, c.user, nil, true, false, true, true, true)
				if err != nil {
					t.Fatal(err)
				}
				if ok != c.served {
					t.Errorf("served %v (miss %+v); want %v", ok, diag, c.served)
				}
				allowed, drift := mocknoise.New(httpNoiseAdapter{}, true, true).StrictReject(m, []byte(c.live), c.user)
				if allowed != c.served {
					t.Errorf("mock serving's body check allowed %v (drift %v); want %v", allowed, drift, c.served)
				}
			})
			t.Run("switch on", func(t *testing.T) {
				m := recorded("m", "/x", form, c.rec)
				m.Noise = c.noise
				if got, diag := strictMatch(t, sent("/x", form, c.live), nil, c.user, nil, m); (got != nil) != c.served {
					t.Errorf("served %v (differs %v); want %v", got != nil, differs(diag), c.served)
				}
			})
		})
	}
}

// A form key keploy.yml's patterns guard is one key however the call escapes
// it: a client that starts or stops escaping brackets, or writes a space as
// "+" or "%20", on replay, a recording that spells the key both ways, and a
// call that adds a second spelling. Both values inside the patterns are
// served and a live value outside them is refused, with the switch on and
// off, and the switch-off miss names the field. Before, with the switch off,
// the live values were looked up by the recorded key's raw text, so such a
// key counted as dropped on replay and was refused, its miss saying nothing
// differed, and a live value outside the patterns under a second spelling
// was not read, so the call was served.
func TestAGuardedFormKeyIsOneKeyHoweverItIsEscaped(t *testing.T) {
	for _, c := range []struct {
		name, entry, pat, rec, live string
		served                      bool
	}{
		{"brackets escaped at record, raw on replay, both inside", "user[id]", `^\d+$`, `user%5Bid%5D=1&x=a`, `user[id]=2&x=a`, true},
		{"brackets escaped at record, raw on replay, the live value outside", "user[id]", `^\d+$`, `user%5Bid%5D=1&x=a`, `user[id]=evil&x=a`, false},
		{"brackets raw at record, escaped on replay, both inside", "user[id]", `^\d+$`, `user[id]=1&x=a`, `user%5Bid%5D=2&x=a`, true},
		{"brackets raw at record, escaped on replay, the live value outside", "user[id]", `^\d+$`, `user[id]=1&x=a`, `user%5Bid%5D=evil&x=a`, false},
		{"a space as + at record, %20 on replay, both inside", "first name", `^[a-z]+$`, `first+name=ann&x=a`, `first%20name=bob&x=a`, true},
		{"a space as + at record, %20 on replay, the live value outside", "first name", `^[a-z]+$`, `first+name=ann&x=a`, `first%20name=Bob1&x=a`, false},
		{"a key spelled both ways at record, both inside", "user[id]", `^\d+$`, `user%5Bid%5D=1&user[id]=2&x=a`, `user[id]=3&user[id]=4&x=a`, true},
		{"a key spelled both ways at record, a live value outside", "user[id]", `^\d+$`, `user%5Bid%5D=1&user[id]=2&x=a`, `user[id]=3&user%5Bid%5D=evil&x=a`, false},
		{"a key spelled both ways at record, outside the patterns, once on replay", "user[id]", `^\d+$`, `user%5Bid%5D=abcd&user[id]=abcd&x=a`, `user[id]=abcd&x=a`, false},
		{"a second spelling on replay, the live value outside", "id", `^\d+$`, `id=1&x=a`, `id=1&%69d=evil&x=a`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			holdsGuardedFormKey(t, c.entry, c.pat, c.rec, c.live, c.served, true)
		})
	}
}

// A form key keploy.yml's patterns guard keeps its values however the call
// escapes or orders them: they are compared as the application sent them
// (URL-decoded), in any order, as the switch compares them, so a repeated
// key's values reordered on replay, or a space in a value written as "%20"
// and then "+", are the same values, even outside the patterns. A changed
// value is refused, with the switch on and off, and the switch-off miss
// names the field. Before, with the switch off, the raw values were compared
// in order, so such a call was refused, its miss saying nothing differed.
func TestAGuardedFormKeyKeepsItsValuesHoweverTheyAreEscapedOrOrdered(t *testing.T) {
	for _, c := range []struct {
		name, rec, live string
		served, named   bool
	}{
		{"a key spelled both ways at record, its values reordered on replay", `user%5Bid%5D=a&user[id]=b&x=1`, `user[id]=b&user[id]=a&x=1`, true, false},
		{"a key spelled both ways at record, a value changed on replay", `user%5Bid%5D=a&user[id]=b&x=1`, `user[id]=evil&user[id]=a&x=1`, false, true},
		{"a repeated key's values reordered on replay", `user[id]=a&user[id]=b&x=1`, `user[id]=b&user[id]=a&x=1`, true, false},
		{"a space as %20 at record, + on replay", `user[id]=a%20b&x=1`, `user[id]=a+b&x=1`, true, false},
		{"a space as %20 at record, + on replay, the value changed", `user[id]=a%20b&x=1`, `user[id]=a+bc&x=1`, false, true},
		// In a body url.ParseQuery does not read (a value or a key that
		// does not decode, or a ';'), values are the same only as written,
		// in order: the switch reads such a body, changed, as changed
		// whole, and names no field.
		{"a value that does not decode, the same on replay", `user[id]=%zz&x=1`, `user[id]=%zz&x=1`, true, false},
		{"a value that does not decode, changed on replay", `user[id]=%zz&x=1`, `user[id]=%zy&x=1`, false, false},
		{"values that do not decode, reordered on replay", `user[id]=%zz&user[id]=a&x=1`, `user[id]=a&user[id]=%zz&x=1`, false, false},
		{"values reordered beside a value that does not decode", `user[id]=a&user[id]=b&y=%zz`, `user[id]=b&user[id]=a&y=%zz`, false, false},
		{"a ';' escaped at record only", `user[id]=a%3Bb&x=1`, `user[id]=a;b&x=1`, false, false},
		{"a ';' escaped on replay only", `user[id]=a;b&x=1`, `user[id]=a%3Bb&x=1`, false, false},
		{"values holding a ';' reordered on replay", `user[id]=a;b&user[id]=c&x=1`, `user[id]=c&user[id]=a;b&x=1`, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			holdsGuardedFormKey(t, "user[id]", `^\d+$`, c.rec, c.live, c.served, c.named)
		})
	}
}

// holdsGuardedFormKey checks that a call with the form body live is served
// (served) by a recording of the form body rec, a keploy.yml request-body
// entry holding entry to the pattern pat: with the switch off by the match
// and by mock serving's body check, and with it on. Where it is refused and
// named, the switch-off miss names the field: the reason by the key's shape,
// as the switch's does (a key like these may carry a value), the live value
// outside the pattern, a string of 4 bytes among them, and the recorded
// ones beside them where rec's values of the key are outside it too, each
// side's shapes once, and the diff by its name.
func holdsGuardedFormKey(t *testing.T, entry, pat, rec, live string, served, named bool) {
	t.Helper()
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	user := requestBodyNoise(map[string][]string{entry: {pat}}).Body
	t.Run("switch off", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		m := recorded("m", "/x", form, rec)
		db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
		ok, _, diag, err := newHTTP().match(context.Background(), sent("/x", form, live), db, nil, user, nil, true, false, true, true, true)
		if err != nil {
			t.Fatal(err)
		}
		if ok != served {
			t.Errorf("served %v (miss %+v); want %v", ok, diag, served)
		}
		if allowed, drift := mocknoise.New(httpNoiseAdapter{}, true, true).StrictReject(m, []byte(live), user); allowed != served {
			t.Errorf("mock serving's body check allowed %v (drift %v); want %v", allowed, drift, served)
		}
		if ok || served || !named {
			return
		}
		request, _ := http.NewRequest("POST", "http://api.example/x", strings.NewReader(live))
		request.Header.Set("Content-Type", form["Content-Type"])
		r := newHTTP().buildHTTPMismatchReport(request, []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, user, diag)
		if r == nil {
			t.Fatal("no miss report")
		}
		field := "body." + models.KeyShape(entry)
		// Each side's values outside the pattern, by their lengths, each
		// length once: "the live value (...)", "the recorded values (...)".
		outside := func(side, body string) (string, int) {
			values, _ := url.ParseQuery(body)
			var shapes []string
			n := 0
			for _, v := range values[entry] {
				if shape := matcher.StringShape(len(v)); !regexp.MustCompile(pat).MatchString(v) {
					n++
					if !slices.Contains(shapes, shape) {
						shapes = append(shapes, shape)
					}
				}
			}
			switch n {
			case 0:
				return "", 0
			case 1:
				return "the " + side + " value (" + shapes[0] + ")", 1
			}
			return "the " + side + " values (" + strings.Join(shapes, ", ") + ")", n
		}
		liveValues, nLive := outside("live", live)
		recValues, nRec := outside("recorded", rec)
		if nLive == 0 || !strings.Contains(liveValues, matcher.StringShape(4)) {
			t.Fatalf("live %q holds %q outside %q; want a string of 4 bytes among them", live, liveValues, pat)
		}
		values := liveValues
		if nRec > 0 {
			values += " and " + recValues
		}
		why := matcher.PatternValuesWhy(field, []string{pat}, values, nLive+nRec > 1)
		want := []models.FieldReason{{Field: field, Why: why}}
		if !reflect.DeepEqual(r.FieldReasons, want) || !strings.Contains(r.Diff, "body."+entry+":") {
			t.Errorf("reasons %v, diff %q; want %v and body.%s named", r.FieldReasons, r.Diff, want, entry)
		}
	})
	t.Run("switch on", func(t *testing.T) {
		m := recorded("m", "/x", form, rec)
		if got, diag := strictMatch(t, sent("/x", form, live), nil, user, nil, m); (got != nil) != served {
			t.Errorf("served %v (differs %v); want %v", got != nil, differs(diag), served)
		}
	})
}

// A form key no keploy.yml entry with patterns guards is read by its raw
// text, its raw values compared in order, as before: an escaped key whose
// recorded value the obfuscator redacted, writing a pattern for the key's
// raw key=value segment beside one for the value, stays excused, with no
// entry and beside a key an entry guards, with the switch on and off; and
// with the switch off, mock serving's body check under strict enforcement
// still refuses such a key's values reordered or escaped another way.
func TestAFormKeyNoEntryGuardsIsReadByItsRawText(t *testing.T) {
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	noise := []string{`^REDACTED_abc$`, `^user%5Bsecret%5D=[^&]+$`}
	rec := `user%5Bsecret%5D=REDACTED_abc&token=t-1`
	for _, c := range []struct {
		name, live string
		user       map[string][]string
	}{
		{"no entry", `user%5Bsecret%5D=s3cr3t&token=t-1`, nil},
		{"beside a key an entry guards", `user%5Bsecret%5D=s3cr3t&token=t-2`, requestBodyNoise(map[string][]string{"token": {`^t-\d$`}}).Body},
	} {
		t.Run(c.name, func(t *testing.T) {
			live := c.live
			t.Run("switch off", func(t *testing.T) {
				t.Setenv(provenswitch.Env, "")
				m := recorded("m", "/x", form, rec)
				m.Noise = noise
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
				ok, _, diag, err := newHTTP().match(context.Background(), sent("/x", form, live), db, nil, c.user, nil, true, false, true, true, true)
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					t.Errorf("refused (miss %+v)", diag)
				}
				if allowed, drift := mocknoise.New(httpNoiseAdapter{}, true, true).StrictReject(m, []byte(live), c.user); !allowed {
					t.Errorf("mock serving's body check refused it (drift %v)", drift)
				}
			})
			t.Run("switch on", func(t *testing.T) {
				m := recorded("m", "/x", form, rec)
				m.Noise = noise
				if got, diag := strictMatch(t, sent("/x", form, live), nil, c.user, nil, m); got == nil {
					t.Errorf("refused (differs %v)", differs(diag))
				}
			})
		})
	}
	tok := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}}).Body
	for _, c := range []struct{ name, rec, live string }{
		{"its values reordered", `x=a&x=b&token=t-1`, `x=b&x=a&token=t-1`},
		{"a value escaped another way", `x=a+b&token=t-1`, `x=a%20b&token=t-1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			if allowed, _ := mocknoise.New(httpNoiseAdapter{}, true, true).StrictReject(recorded("m", "/x", form, c.rec), []byte(c.live), tok); allowed {
				t.Error("mock serving's body check allowed it")
			}
		})
	}
}

// With the switch off, a call refused for one field while a redacted
// recorded field's live value is inside its keploy.yml pattern names no
// pattern for the redacted field: the patterns did not refuse it.
func TestWithTheSwitchOffARedactedFieldInsideItsPatternIsNotNamed(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	user := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}}).Body
	hdr := map[string]string{"Content-Type": "application/json"}
	m := recorded("m", "/x", hdr, `{"token":"REDACTED_abc","n":1}`)
	m.Noise = []string{`^REDACTED_abc$`}
	live := `{"token":"t-1","n":2}`
	db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
	ok, _, diag, err := newHTTP().match(context.Background(), sent("/x", hdr, live), db, nil, user, nil, true, false, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("served")
	}
	request, _ := http.NewRequest("POST", "http://api.example/x", strings.NewReader(live))
	request.Header.Set("Content-Type", hdr["Content-Type"])
	r := newHTTP().buildHTTPMismatchReport(request, []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, user, diag)
	if r == nil {
		t.Fatal("no miss report")
	}
	if len(r.FieldReasons) != 0 {
		t.Errorf("reasons %v; want none", r.FieldReasons)
	}
}

// With the switch off, the reasons of a miss read keploy.yml's noise only:
// noise learned on the closest mock that names a guarded field's container
// does not hide the field the patterns refused.
func TestWithTheSwitchOffLearnedNoiseDoesNotHideAPatternRefusal(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	user := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}}).Body
	hdr := map[string]string{"Content-Type": "application/json"}
	m := recorded("m", "/x", hdr, `{"a":{"token":"t-1"},"n":1}`)
	m.Spec.ReqBodyNoise = map[string][]string{"body.a": {}}
	live := `{"a":{"token":"evil"},"n":1}`
	db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
	ok, _, diag, err := newHTTP().match(context.Background(), sent("/x", hdr, live), db, nil, user, nil, true, false, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("served")
	}
	request, _ := http.NewRequest("POST", "http://api.example/x", strings.NewReader(live))
	request.Header.Set("Content-Type", hdr["Content-Type"])
	r := newHTTP().buildHTTPMismatchReport(request, []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, user, diag)
	if r == nil {
		t.Fatal("no miss report")
	}
	want := []models.FieldReason{{Field: "body.a.token", Why: matcher.PatternWhy("body.a.token", []string{`^t-\d$`}, "live", matcher.StringShape(4))}}
	if !reflect.DeepEqual(r.FieldReasons, want) {
		t.Errorf("reasons %v; want %v", r.FieldReasons, want)
	}
}

// Every recorded value of a JSON array's guarded field that the obfuscator
// did not redact is held to keploy.yml's patterns, wherever it stands: one
// outside them is refused beside a redacted one, in either order, with the
// switch on and off. Before, with the switch off, only the last recorded
// value decided whether the field was redacted, so the redacted one last
// excused an unredacted one outside the patterns.
func TestEveryUnredactedRecordedValueOfAnArrayIsHeldWithTheSwitchOnAndOff(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	tok := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}}).Body
	live := `{"items":[{"token":"t-1"},{"token":"t-2"}]}`
	for _, c := range []struct {
		rec    string
		served bool
	}{
		{`{"items":[{"token":"bbbbbb"},{"token":"REDACTED_abc"}]}`, false},
		{`{"items":[{"token":"REDACTED_abc"},{"token":"bbbbbb"}]}`, false},
		{`{"items":[{"token":"t-3"},{"token":"REDACTED_abc"}]}`, true},
		{`{"items":[{"token":"REDACTED_abc"},{"token":"t-3"}]}`, true},
	} {
		t.Run(c.rec, func(t *testing.T) {
			t.Run("switch off", func(t *testing.T) {
				t.Setenv(provenswitch.Env, "")
				m := recorded("m", "/x", ct, c.rec)
				m.Noise = []string{`^REDACTED_abc$`}
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
				ok, _, _, err := newHTTP().match(context.Background(), sent("/x", ct, live), db, nil, tok, nil, true, false, true, true, true)
				if err != nil {
					t.Fatal(err)
				}
				if ok != c.served {
					t.Errorf("served %v; want %v", ok, c.served)
				}
			})
			t.Run("switch on", func(t *testing.T) {
				m := recorded("m", "/x", ct, c.rec)
				m.Noise = []string{`^REDACTED_abc$`}
				if got, diag := strictMatch(t, sent("/x", ct, live), nil, tok, nil, m); (got != nil) != c.served {
					t.Errorf("served %v (differs %v); want %v", got != nil, differs(diag), c.served)
				}
			})
		})
	}
}

// With the switch off, a call refused under strict enforcement for a field
// keploy.yml's request-body patterns guard, its live value outside them,
// has a miss that names the field, the patterns and the side, giving only
// the value's kind and length, as the switch's does: a form's key, redacted
// or not, and a redacted JSON field. Before, the miss gave no reason, and a
// form's said nothing differed.
func TestWithTheSwitchOffAPatternRefusalNamesTheField(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	tok := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}}).Body
	want := []models.FieldReason{{Field: "body.token", Why: matcher.PatternWhy("body.token", []string{`^t-\d$`}, "live", matcher.StringShape(4))}}
	for _, c := range []struct {
		name, ctype, rec, live string
		noise                  []string
	}{
		{"a form key", "application/x-www-form-urlencoded", `token=t-1&x=a`, `token=evil&x=a`, nil},
		{"a form key beside another that changed", "application/x-www-form-urlencoded", `token=t-1&x=a`, `token=evil&x=b`, nil},
		{"a redacted form key", "application/x-www-form-urlencoded", `token=REDACTED_abc&x=a`, `token=evil&x=a`, []string{`^REDACTED_abc$`, `^token=[^&]+$`}},
		{"a redacted JSON field", "application/json", `{"token":"REDACTED_abc","n":1}`, `{"token":"evil","n":1}`, []string{`^REDACTED_abc$`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			hdr := map[string]string{"Content-Type": c.ctype}
			m := recorded("m", "/x", hdr, c.rec)
			m.Noise = c.noise
			db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
			ok, _, diag, err := newHTTP().match(context.Background(), sent("/x", hdr, c.live), db, nil, tok, nil, true, false, true, true, true)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Fatal("served")
			}
			request, _ := http.NewRequest("POST", "http://api.example/x", strings.NewReader(c.live))
			request.Header.Set("Content-Type", c.ctype)
			r := newHTTP().buildHTTPMismatchReport(request, []byte(c.live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, tok, diag)
			if r == nil {
				t.Fatal("no miss report")
			}
			if !reflect.DeepEqual(r.FieldReasons, want) {
				t.Errorf("reasons %v; want %v", r.FieldReasons, want)
			}
			var named []string
			for _, d := range r.FieldDiffs {
				if strings.HasPrefix(d.Path, "body.") {
					named = append(named, d.Path)
				}
			}
			if !reflect.DeepEqual(named, []string{"body.token"}) || strings.Contains(r.Diff, "no field-level differences") {
				t.Errorf("body diffs %v, diff %q; want body.token named once", named, r.Diff)
			}
		})
	}
}

// A miss in another phase (here the path differs) gives no pattern's
// reason: there the patterns refused nothing.
func TestWithTheSwitchOffAMissOfAnotherPhaseNamesNoPattern(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	tok := requestBodyNoise(map[string][]string{"token": {`^t-\d$`}}).Body
	m := recorded("m", "/x", form, `token=t-1&x=a`)
	live := `token=evil&x=a`
	db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
	ok, _, diag, err := newHTTP().match(context.Background(), sent("/y", form, live), db, nil, tok, nil, true, false, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if ok || diag == nil || diag.phase == models.MatchPhaseStrict {
		t.Fatalf("served %v, miss %+v; want a miss before the strict phase", ok, diag)
	}
	request, _ := http.NewRequest("POST", "http://api.example/y", strings.NewReader(live))
	request.Header.Set("Content-Type", form["Content-Type"])
	if r := newHTTP().buildHTTPMismatchReport(request, []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, tok, diag); r == nil || len(r.FieldReasons) > 0 {
		t.Fatalf("report %+v; want no field reasons", r)
	}
}

// A keploy.yml entry with no patterns holds no form key: with the switch off
// under strict enforcement a form key it names by a field name, changed, is
// still served, as before; only an entry with patterns holds a key's values.
func TestWithTheSwitchOffAWholeFieldEntryHoldsNoFormKey(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	all := requestBodyNoise(map[string][]string{"token": {}}).Body
	for _, c := range [][2]string{{`a.token=1&x=a`, `a.token=2&x=a`}, {`Token=1&x=a`, `Token=2&x=a`}} {
		m := recorded("m", "/x", form, c[0])
		db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
		ok, _, diag, err := newHTTP().match(context.Background(), sent("/x", form, c[1]), db, nil, all, nil, true, false, true, true, true)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("%s to %s: not served (miss %+v)", c[0], c[1], diag)
		}
	}
}
