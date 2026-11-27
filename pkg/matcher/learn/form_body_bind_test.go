package learn

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestGivenRequestReadsFormURLEncodedBody verifies that GivenRequest parses
// raw wire application/x-www-form-urlencoded request bodies into bindable
// body.<key> leaves ahead of the raw body, decoding per key/value so encoded
// '&' (%26), '=' (%3D), '+' (%2B), and '%' (%25) stay intact without phantom
// keys or truncation, while keeping JSON bodies sent under a form Content-Type
// as JSON and handling repeated, dotted, bracketed, empty, and invalid-escape
// bodies safely.
func TestGivenRequestReadsFormURLEncodedBody(t *testing.T) {
	formReq := func(recCT, sentCT, recBody, sentBody string) *models.TestRequest {
		return &models.TestRequest{
			Test: "post-oauth-par-2",
			Parts: []models.TestRequestPart{
				{Name: "url", Recorded: "http://localhost:8083/oauth/par", Sent: "http://localhost:8083/oauth/par"},
				{Name: "header.Content-Type", Recorded: recCT, Sent: sentCT},
				{Name: "body", Recorded: recBody, Sent: sentBody},
			},
		}
	}

	rawRec := url.Values{
		"client_id":       {"demo-cli"},
		"redirect_uri":    {"https://app.example/cb?state=abc&nonce=1794207702156240454"},
		"installation_id": {"cli-inst-1794207702156240454"},
		"device_name":     {"e2e ide box"},
		"code_challenge":  {"AbCdEf123456_rec+val"},
		"note":            {"x&y=z"},
		"pct":             {"50% off"},
		"tag":             {"b", "a"},
		"user[id]":        {"u-1"},
		"user.name":       {"alice"},
		"":                {"empty-key-val"},
	}.Encode()
	rawSent := url.Values{
		"client_id":       {"demo-cli"},
		"redirect_uri":    {"https://app.example/cb?state=abc&nonce=1794207771159240454"},
		"installation_id": {"cli-inst-1794207771159240454"},
		"device_name":     {"e2e ide box"},
		"code_challenge":  {"XyZwVu987654_live+val"},
		"note":            {"x&y=z"},
		"pct":             {"50% off"},
		"tag":             {"b", "a"},
		"user[id]":        {"u-2"},
		"user.name":       {"bob"},
		"":                {"empty-key-val"},
		"only_sent":       {"1"},
	}.Encode()

	for _, tc := range []struct {
		name, ct string
	}{
		{"exact header", "application/x-www-form-urlencoded"},
		{"parameterized mixed-case header", "Application/X-WWW-Form-Urlencoded; charset=UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := GivenRequest(formReq(tc.ct, tc.ct, rawRec, rawSent))
			if e.Unread != "" {
				t.Fatalf("Unread = %q; want readable", e.Unread)
			}
			byPath := map[string]Seen{}
			bodyIdx, fieldIdx := -1, -1
			for i, s := range e.Seen {
				byPath[s.Leaf.Path] = s
				if s.Leaf.Path == "body" {
					bodyIdx = i
				}
				if s.Leaf.Path == "body.installation_id" {
					fieldIdx = i
				}
			}
			if fieldIdx < 0 || bodyIdx < 0 || fieldIdx >= bodyIdx {
				t.Fatalf("expected body.installation_id (%d) ahead of raw body (%d)", fieldIdx, bodyIdx)
			}
			if s := byPath["body.redirect_uri"]; s.Leaf.Value != "https://app.example/cb?state=abc&nonce=1794207702156240454" ||
				s.Live != "https://app.example/cb?state=abc&nonce=1794207771159240454" || s.Leaf.Unnamed {
				t.Errorf("body.redirect_uri = %+v; want intact URL with encoded &", s)
			}
			if _, phantom := byPath["body.nonce"]; phantom {
				t.Errorf("phantom body.nonce leaf invented from encoded & inside redirect_uri")
			}
			if s := byPath["body.note"]; s.Leaf.Value != "x&y=z" || s.Live != "x&y=z" || s.Leaf.Unnamed {
				t.Errorf("body.note = %+v; want intact x&y=z", s)
			}
			if _, phantom := byPath["body.y"]; phantom {
				t.Errorf("phantom body.y leaf invented from encoded &/= inside note")
			}
			if s := byPath["body.device_name"]; s.Leaf.Value != "e2e ide box" || s.Leaf.Unnamed {
				t.Errorf("body.device_name = %+v; want space-decoded named leaf", s)
			}
			if s := byPath["body.code_challenge"]; s.Leaf.Value != "AbCdEf123456_rec+val" || s.Live != "XyZwVu987654_live+val" || s.Leaf.Unnamed {
				t.Errorf("body.code_challenge = %+v; want '+' preserved without double-decoding", s)
			}
			if s := byPath["body.pct"]; s.Leaf.Value != "50% off" || s.Live != "50% off" || s.Leaf.Unnamed {
				t.Errorf("body.pct = %+v; want literal '%%' preserved", s)
			}
			if s := byPath["body.installation_id"]; s.Leaf.Value != "cli-inst-1794207702156240454" || s.Live != "cli-inst-1794207771159240454" || s.Leaf.Unnamed {
				t.Errorf("body.installation_id = %+v; want named leaf", s)
			}
			if s, ok := byPath["body.tag"]; !ok || s.Leaf.Value != "a, b" || !s.Leaf.Unnamed {
				t.Errorf("repeated key body.tag = %+v (ok=%v); want sorted joined Unnamed leaf", s, ok)
			}
			if s, ok := byPath["body.user[id]"]; !ok || !s.Leaf.Unnamed {
				t.Errorf("bracketed key body.user[id] = %+v (ok=%v); want Unnamed leaf", s, ok)
			}
			if s, ok := byPath["body.user.name"]; !ok || !s.Leaf.Unnamed {
				t.Errorf("dotted key body.user.name = %+v (ok=%v); want Unnamed leaf", s, ok)
			}
			if s, ok := byPath["body."]; !ok || !s.Leaf.Unnamed {
				t.Errorf("empty key body. = %+v (ok=%v); want Unnamed leaf", s, ok)
			}
			names, ok := byPath["request{names}"]
			if !ok || !strings.Contains(names.Leaf.Value, "installation_id") {
				t.Errorf("request{names} = %+v (ok=%v); want recorded form key names included", names, ok)
			}
			if strings.Contains(names.Leaf.Value, "nonce") || strings.Contains(names.Live, "nonce") ||
				strings.Contains(names.Leaf.Value, "\ny\n") {
				t.Errorf("request{names} = rec:%q live:%q; must not contain phantom keys from encoded &", names.Leaf.Value, names.Live)
			}
			if strings.Contains(names.Leaf.Value, "only_sent") || !strings.Contains(names.Live, "only_sent") {
				t.Errorf("request{names} = rec:%q live:%q; want only_sent in sent names only", names.Leaf.Value, names.Live)
			}
		})
	}

	// JSON body sent under a form Content-Type (e.g. jQuery $.ajax default)
	// must continue to produce JSON leaves, never a junk form key.
	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"application/x-www-form-urlencoded; charset=UTF-8",
	} {
		t.Run("JSON body under "+ct+" stays JSON", func(t *testing.T) {
			e := GivenRequest(formReq(ct, ct, `{"userId":"u-rec-1","qty":2}`, `{"userId":"u-live-2","qty":2}`))
			if e.Unread != "" {
				t.Fatalf("Unread = %q; want readable", e.Unread)
			}
			byPath := map[string]Seen{}
			for _, s := range e.Seen {
				byPath[s.Leaf.Path] = s
			}
			if s, ok := byPath["body.userId"]; !ok || s.Leaf.Value != "u-rec-1" || s.Live != "u-live-2" || s.Leaf.Unnamed {
				t.Fatalf("body.userId = %+v (ok=%v); want JSON leaf u-rec-1 -> u-live-2", s, ok)
			}
			if s, ok := byPath["body.qty"]; !ok || s.Leaf.Value != "2" || s.Live != "2" || s.Leaf.Unnamed {
				t.Fatalf("body.qty = %+v (ok=%v); want JSON leaf 2", s, ok)
			}
			if _, junk := byPath[`body.{"userId":"u-rec-1","qty":2}`]; junk {
				t.Fatalf("JSON body under %s must not produce junk form leaf", ct)
			}
		})
	}

	// Sent side with non-form media type does not parse sent body as form.
	t.Run("sent non-form media type leaves live empty and omits sentNames", func(t *testing.T) {
		e := GivenRequest(formReq("application/x-www-form-urlencoded; charset=utf-8", "text/plain", "k=v1", "k=v2&extra=1"))
		byPath := map[string]Seen{}
		for _, s := range e.Seen {
			byPath[s.Leaf.Path] = s
		}
		if s, ok := byPath["body.k"]; !ok || s.Leaf.Value != "v1" || s.Live != "" {
			t.Fatalf("body.k = %+v (ok=%v); want recorded v1 and empty Live when sent is text/plain", s, ok)
		}
		if names := byPath["request{names}"]; strings.Contains(names.Live, "extra") {
			t.Fatalf("request{names}.Live = %q; want no form names parsed from text/plain sent body", names.Live)
		}
	})

	// Repeated in recorded only must also mark the leaf Unnamed so learning
	// never names a bind that replay's FormLeafAt would refuse.
	t.Run("repeated key in recorded only is Unnamed", func(t *testing.T) {
		e := GivenRequest(formReq("application/x-www-form-urlencoded", "application/x-www-form-urlencoded", "k=v1&k=v2", "k=v1"))
		for _, s := range e.Seen {
			if s.Leaf.Path == "body.k" {
				if !s.Leaf.Unnamed || s.Leaf.Value != "v1, v2" || s.Live != "v1" {
					t.Fatalf("body.k = %+v; want Unnamed recorded-only repeat", s)
				}
				return
			}
		}
		t.Fatalf("body.k leaf missing from %+v", e.Seen)
	})

	// Repeated in sent only must also mark the leaf Unnamed so replay never
	// tries to bind a single-value place against a multi-value live form.
	t.Run("repeated key in sent only is Unnamed", func(t *testing.T) {
		e := GivenRequest(formReq("application/x-www-form-urlencoded", "application/x-www-form-urlencoded", "k=v1", "k=v1&k=v2"))
		for _, s := range e.Seen {
			if s.Leaf.Path == "body.k" {
				if !s.Leaf.Unnamed {
					t.Fatalf("body.k = %+v; want Unnamed when sent repeats key", s)
				}
				return
			}
		}
		t.Fatalf("body.k leaf missing from %+v", e.Seen)
	})

	// Empty and invalid raw bodies do not panic and fall back cleanly to raw body.
	for _, tc := range []struct {
		name, ct, body string
	}{
		{"empty body", "application/x-www-form-urlencoded", ""},
		{"invalid percent escape in exact CT form", "application/x-www-form-urlencoded", "discount=50%&name=a+b%20c"},
		{"invalid percent escape in parameterized CT form", "application/x-www-form-urlencoded; charset=utf-8", "installation_id=%zz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := GivenRequest(formReq(tc.ct, tc.ct, tc.body, tc.body))
			if e.Unread != "" {
				t.Fatalf("Unread = %q; want readable", e.Unread)
			}
			for _, s := range e.Seen {
				if strings.HasPrefix(s.Leaf.Path, "body.") {
					t.Errorf("unexpected form leaf %q for %s", s.Leaf.Path, tc.name)
				}
			}
		})
	}
}

// TestFormURLEncodedClientRequestBindsInDecideAndClassify reproduces the
// OAuth PAR scenario: a client test sends application/x-www-form-urlencoded
// parameters to POST /oauth/par, and a downstream DB/HTTP call carries the
// UnixNano-derived installation_id or random PKCE code_challenge. Both Decide
// (with an adapter PlaceOf) and ClassifyArrived must bind at test case
// post-oauth-par-2 body.<field> rather than refusing with no place named.
func TestFormURLEncodedClientRequestBindsInDecideAndClassify(t *testing.T) {
	rNanos := int64(1794207702156240454)
	rReq := time.Unix(0, rNanos).Add(131 * time.Microsecond)
	gap := 69*time.Second + 3*time.Millisecond
	fNanos, fReq := rNanos+gap.Nanoseconds(), rReq.Add(gap)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	ns := func(v int64) string { return strconv.FormatInt(v, 10) }

	rInst, fInst := "cli-inst-"+ns(rNanos), "cli-inst-"+ns(fNanos)
	rChal, fChal := "pkceChallengeRec987654321+abc", "pkceChallengeLive123456789+xyz"

	parReq := func(ct, inst, chal string, at time.Time) *models.HTTPReq {
		body := url.Values{
			"client_id":             {"demo-cli"},
			"redirect_uri":          {"vscode://keploy.keployio/did-authenticate?a=1&b=2"},
			"code_challenge":        {chal},
			"code_challenge_method": {"S256"},
			"state":                 {"e2e-state-1"},
			"installation_id":       {inst},
			"device_name":           {"e2e-ide-box"},
			"device_os":             {"linux"},
		}.Encode()
		return &models.HTTPReq{
			Method:    "POST",
			URL:       "http://localhost:8083/oauth/par",
			Timestamp: at,
			Header:    map[string]string{"Content-Type": ct, "Host": "localhost:8083"},
			Body:      body,
		}
	}

	placeAll := func(l Leaf, _ bool) (string, bool) { return VarPlace(l, true) }

	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"application/x-www-form-urlencoded; charset=utf-8",
	} {
		t.Run(ct, func(t *testing.T) {
			earlier := clientPlaced(t, "post-oauth-par-2", parReq(ct, rInst, rChal, rReq), parReq(ct, fInst, fChal, fReq), fReq)

			// 1. Decide on UnixNano-derived installation_id word -> bound at body.installation_id#2.
			inInst := iCall("body.docs.msgs.0.installation_id", String, rInst, fInst, rCall, fCall, nil, earlier, nil)
			inInst.Drifts = oClientDrifts()
			inInst.PlaceOf = placeAll
			d := oWord(t, Decide(inInst), ns(rNanos), ns(fNanos))
			if d.Outcome != OutcomeValue || !d.From.Client || d.From.Mock != "test case post-oauth-par-2" || d.From.Path != "body.installation_id" {
				t.Fatalf("installation_id Decide: %s (client=%v mock=%q path=%q); want OutcomeValue from test case post-oauth-par-2 body.installation_id",
					oDescribe([]Decision{d}), d.From.Client, d.From.Mock, d.From.Path)
			}
			if got := d.FromAt.String(); got != "body.installation_id#2" {
				t.Fatalf("installation_id FromAt = %q; want body.installation_id#2", got)
			}

			// 2. Decide on code_challenge with '+' -> bound at body.code_challenge (no '+' corruption).
			inChal := iCall("body.docs.msgs.0.code_challenge", String, rChal, fChal, rCall, fCall, nil, earlier, nil)
			inChal.Drifts = oClientDrifts()
			inChal.PlaceOf = placeAll
			d = oWord(t, Decide(inChal), "pkceChallengeRec987654321", "pkceChallengeLive123456789")
			if d.Outcome != OutcomeValue || !d.From.Client || d.From.Mock != "test case post-oauth-par-2" || d.From.Path != "body.code_challenge" {
				t.Fatalf("code_challenge Decide: %s (client=%v mock=%q path=%q); want OutcomeValue from test case post-oauth-par-2 body.code_challenge",
					oDescribe([]Decision{d}), d.From.Client, d.From.Mock, d.From.Path)
			}

			// 3. ClassifyArrived (HTTP adapter path) -> BindAt returns body.code_challenge with exact '+'.
			sh := func(v string) Shape {
				return Shape{Leaves: map[string]Leaf{"body.challenge": {Path: "body.challenge", Noise: "body.challenge", Type: String, Value: v}}}
			}
			ln := ClassifyArrived(oClientDrifts(), []string{"body.challenge"}, sh(fChal), sh(rChal), rCall, fCall, earlier, nil, nil, nil, nil)[0]
			at, live, ok := ln.From.BindAt()
			if ln.Kind != KindVariable || !ln.From.Client || ln.From.Mock != "test case post-oauth-par-2" || !ok || at != "body.code_challenge" || live != fChal {
				t.Fatalf("ClassifyArrived: kind=%v from=%+v bind=(%q,%q,%v); want body.code_challenge=%q",
					ln.Kind, ln.From, at, live, ok, fChal)
			}
		})
	}
}

func TestPairCaseLegacyDecodedFormMatchesRawLiveOutOfOrder(t *testing.T) {
	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"application/x-www-form-urlencoded; charset=utf-8",
	} {
		t.Run(ct, func(t *testing.T) {
			req := func(body string) *models.HTTPReq {
				return &models.HTTPReq{
					Method: "POST",
					URL:    "http://api/signup",
					Header: map[string]string{"Content-Type": ct},
					Body:   body,
				}
			}
			cases := []ClientCase{
				{Name: "post-signup-1", Req: req("email=ann@example.com&plan=pro")},
				{Name: "post-signup-2", Req: req("email=bob@example.com&plan=pro")},
			}
			// Live requests arrive raw (percent-encoded); test both targets
			// against a full two-candidate list so PairCase cannot win by
			// elimination.
			i, ok := PairCase(cases, make([]bool, len(cases)), req("email=bob%40example.com&plan=pro"), true)
			if !ok || cases[i].Name != "post-signup-2" {
				t.Fatalf("bob paired with (%d, %v); want post-signup-2", i, ok)
			}
			j, ok := PairCase(cases, make([]bool, len(cases)), req("email=ann%40example.com&plan=pro"), true)
			if !ok || cases[j].Name != "post-signup-1" {
				t.Fatalf("ann paired with (%d, %v); want post-signup-1", j, ok)
			}

			// Legacy pre-decoded recordings with literal '+' (originally %2B)
			// must beat a space-bearing candidate listed first when paired
			// against a raw live request carrying %2B.
			plusCases := []ClientCase{
				{Name: "post-space", Req: req("email=ann x@example.com&plan=pro")},
				{Name: "post-plus", Req: req("email=ann+x@example.com&plan=pro")},
			}
			k, ok := PairCase(plusCases, make([]bool, len(plusCases)), req("email=ann%2Bx%40example.com&plan=pro"), true)
			if !ok || plusCases[k].Name != "post-plus" {
				t.Fatalf("ann+x paired with (%d, %v); want post-plus", k, ok)
			}
		})
	}
}

func TestFormSeenMarksMissingEqAndInvalidUTF8UnnamedAndBindsHashKey(t *testing.T) {
	mk := func(recBody, liveBody string) *models.TestRequest {
		return &models.TestRequest{
			Test: "post-edge-1",
			Parts: []models.TestRequestPart{
				{Name: "header.Content-Type", Recorded: "application/x-www-form-urlencoded", Sent: "application/x-www-form-urlencoded"},
				{Name: "body", Recorded: recBody, Sent: liveBody},
			},
		}
	}

	t.Run("empty key is Unnamed with reason", func(t *testing.T) {
		e := GivenRequest(mk("=val1&ok=1", "=val2&ok=1"))
		found := false
		for _, s := range e.Seen {
			if s.Leaf.Path == "body." {
				found = true
				if !s.Leaf.Unnamed || s.Leaf.UnnamedWhy != "form key is empty" {
					t.Errorf("empty-key leaf = (%v, %q); want (true, \"form key is empty\")", s.Leaf.Unnamed, s.Leaf.UnnamedWhy)
				}
			}
		}
		if !found {
			t.Fatalf("expected Unnamed body. leaf in %+v", e.Seen)
		}
	})

	t.Run("no equals sign (e.g. BOM-prefixed JSON) is Unnamed", func(t *testing.T) {
		e := GivenRequest(mk("\xef\xbb\xbf{\"a\":1}", "\xef\xbb\xbf{\"a\":2}"))
		found := false
		for _, s := range e.Seen {
			if strings.HasPrefix(s.Leaf.Path, "body.") {
				found = true
				if !s.Leaf.Unnamed || s.Leaf.UnnamedWhy != "form pair has no '='" {
					t.Errorf("leaf %q = (%v, %q); want (true, \"form pair has no '='\")", s.Leaf.Path, s.Leaf.Unnamed, s.Leaf.UnnamedWhy)
				}
			}
		}
		if !found {
			t.Fatalf("expected Unnamed body.* leaf for missing '=' in %+v", e.Seen)
		}
	})

	t.Run("percent-decoded invalid UTF-8 value is Unnamed", func(t *testing.T) {
		e := GivenRequest(mk("n=%FF%FE1", "n=%FF%FE2"))
		found := false
		for _, s := range e.Seen {
			if s.Leaf.Path == "body.n" {
				found = true
				if !s.Leaf.Unnamed || s.Leaf.UnnamedWhy != "form field decodes to invalid UTF-8" {
					t.Errorf("leaf %q = (%v, %q); want (true, \"form field decodes to invalid UTF-8\")", s.Leaf.Path, s.Leaf.Unnamed, s.Leaf.UnnamedWhy)
				}
			}
		}
		if !found {
			t.Fatalf("expected Unnamed body.n leaf in %+v", e.Seen)
		}
	})

	t.Run("key containing '#' binds via quoted VarPlace", func(t *testing.T) {
		rNanos := int64(1764192000123456789)
		gap := 69*time.Second + 3*time.Millisecond
		fNanos := rNanos + gap.Nanoseconds()
		ns := func(v int64) string { return strconv.FormatInt(v, 10) }
		rReq := time.Unix(0, rNanos).Add(131 * time.Microsecond)
		fReq := rReq.Add(gap)
		rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
		rInst, fInst := "cli-inst-"+ns(rNanos), "cli-inst-"+ns(fNanos)
		mkReq := func(body string, at time.Time) *models.HTTPReq {
			return &models.HTTPReq{
				Method:    "POST",
				URL:       "http://localhost:8083/oauth/par",
				Timestamp: at,
				Header:    map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Host": "localhost:8083"},
				Body:      body,
			}
		}
		earlier := clientPlaced(t, "post-hash-1", mkReq("inst%231="+rInst, rReq), mkReq("inst%231="+fInst, fReq), fReq)
		in := iCall("body.docs.msgs.0.installation_id", String, rInst, fInst, rCall, fCall, nil, earlier, nil)
		in.Drifts = oClientDrifts()
		in.PlaceOf = func(l Leaf, _ bool) (string, bool) { return VarPlace(l, true) }
		d := oWord(t, Decide(in), ns(rNanos), ns(fNanos))
		if d.Outcome != OutcomeValue {
			t.Fatalf("Outcome = %v; want OutcomeValue; line=%q", d.Outcome, d.Line)
		}
		if got := d.FromAt.Place.String(); got != `body["inst#1"]` {
			t.Fatalf("FromAt.Place = %q; want body[\"inst#1\"]", got)
		}
	})
}

func TestDecideNamesUnbindableFormKeyAndParseReasons(t *testing.T) {
	rNanos := int64(1764192000123456789)
	gap := 69*time.Second + 3*time.Millisecond
	fNanos := rNanos + gap.Nanoseconds()
	ns := func(v int64) string { return strconv.FormatInt(v, 10) }
	rReq := time.Unix(0, rNanos).Add(131 * time.Microsecond)
	fReq := rReq.Add(gap)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	rInst, fInst := "cli-inst-"+ns(rNanos), "cli-inst-"+ns(fNanos)
	mkReq := func(body string, at time.Time) *models.HTTPReq {
		return &models.HTTPReq{
			Method:    "POST",
			URL:       "http://localhost:8083/oauth/par",
			Timestamp: at,
			Header:    map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Host": "localhost:8083"},
			Body:      body,
		}
	}
	placeAll := func(l Leaf, _ bool) (string, bool) { return VarPlace(l, true) }

	tooManyParams := func(inst string) string {
		var b strings.Builder
		b.WriteString("installation_id=")
		b.WriteString(inst)
		for i := 0; i < 10001; i++ {
			b.WriteString("&p")
			b.WriteString(strconv.Itoa(i))
			b.WriteString("=1")
		}
		return b.String()
	}

	for _, tc := range []struct {
		name     string
		recBody  string
		liveBody string
		wantWhy  string
	}{
		{"repeated key", "installation_id=" + rInst + "&installation_id=" + rInst, "installation_id=" + fInst, "form key repeats"},
		{"dotted key", "user.installation_id=" + rInst, "user.installation_id=" + fInst, "form key holds '.', '[' or ']'"},
		{"bracketed key", "user[0]=" + rInst, "user[0]=" + fInst, "form key holds '.', '[' or ']'"},
		{"invalid percent-escape", "discount=50%&installation_id=" + rInst, "discount=50%&installation_id=" + fInst, "invalid percent-escape"},
		{"legacy pre-decoded percent-escape", "discount=50%&installation_id=" + rInst, "discount=50%25&installation_id=" + fInst, "re-record"},
		{"live sent invalid percent-escape", "discount=50%25&installation_id=" + rInst, "discount=50%&installation_id=" + fInst, "live form body could not be parsed: invalid percent-escape"},
		{"semicolon separator", "a=1;b=2&installation_id=" + rInst, "a=1;b=2&installation_id=" + fInst, "invalid semicolon separator"},
		{"parameter limit", tooManyParams(rInst), tooManyParams(fInst), "exceeds Go URL query parameter limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			earlier := clientPlaced(t, "post-oauth-par-2", mkReq(tc.recBody, rReq), mkReq(tc.liveBody, fReq), fReq)
			in := iCall("body.docs.msgs.0.installation_id", String, rInst, fInst, rCall, fCall, nil, earlier, nil)
			in.Drifts = oClientDrifts()
			in.PlaceOf = placeAll
			d := oWord(t, Decide(in), ns(rNanos), ns(fNanos))
			if d.FromAt.Place.Part != 0 {
				t.Fatalf("FromAt = %q; want empty (unbindable)", d.FromAt.String())
			}
			if !strings.Contains(d.Line, tc.wantWhy) || !strings.Contains(d.From.UnbindableWhy, tc.wantWhy) {
				t.Fatalf("line=%q unbindableWhy=%q; want substring %q", d.Line, d.From.UnbindableWhy, tc.wantWhy)
			}
		})
	}
}
