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
	const exactCT = "application/x-www-form-urlencoded"
	const charsetCT = "application/x-www-form-urlencoded; charset=utf-8"

	type recSpec struct {
		name   string
		body   string
		legacy bool // true => stored pre-decoded by an older build (RawFormBody=false)
	}
	type livePair struct{ body, want string }
	type pairRow struct {
		name  string
		cts   []string
		recs  []recSpec
		lives []livePair
	}

	rows := []pairRow{
		// Legacy pre-decoded exact-CT recordings (RawFormBody=false; older builds
		// only decoded exact CT, never charset-parameterized CT) paired against
		// raw live requests across all 4 recording/arrival orders.
		{
			name: "legacy @",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "ann", body: "email=ann@example.com&plan=pro", legacy: true},
				{name: "bob", body: "email=bob@example.com&plan=pro", legacy: true},
			},
			lives: []livePair{
				{"email=ann%40example.com&plan=pro", "ann"},
				{"email=bob%40example.com&plan=pro", "bob"},
			},
		},
		{
			name: "legacy plus/space",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "plus", body: "q=a+b&k=1", legacy: true},
				{name: "space", body: "q=a b&k=1", legacy: true},
			},
			lives: []livePair{
				{"q=a%2Bb&k=1", "plus"},
				{"q=a+b&k=1", "space"},
			},
		},
		{
			name: "legacy email plus/space",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "post-plus", body: "email=ann+x@example.com&plan=pro", legacy: true},
				{name: "post-space", body: "email=ann x@example.com&plan=pro", legacy: true},
			},
			lives: []livePair{
				{"email=ann%2Bx%40example.com&plan=pro", "post-plus"},
				{"email=ann+x%40example.com&plan=pro", "post-space"},
			},
		},
		{
			name: "legacy literal %20 vs space",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "lit", body: "q=a%20b&k=1", legacy: true},
				{name: "space", body: "q=a b&k=1", legacy: true},
			},
			lives: []livePair{
				{"q=a%2520b&k=1", "lit"},
				{"q=a+b&k=1", "space"},
			},
		},
		{
			name: "legacy # key",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "v1", body: "inst#1=v1", legacy: true},
				{name: "v2", body: "inst#1=v2", legacy: true},
			},
			lives: []livePair{
				{"inst%231=v1", "v1"},
				{"inst%231=v2", "v2"},
			},
		},
		{
			name: "legacy encoded delimiter a=1%262 vs a=1%2B2",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "amp", body: "a=1&2", legacy: true},
				{name: "plus", body: "a=1+2", legacy: true},
			},
			lives: []livePair{
				{"a=1%262", "amp"},
				{"a=1%2B2", "plus"},
			},
		},
		{
			name: "legacy JSON-under-form plus/space",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "space", body: `{"q":"a b","k":"1"}`, legacy: true},
				{name: "plus", body: `{"q":"a+b","k":"1"}`, legacy: true},
			},
			lives: []livePair{
				{`{"q":"a+b","k":"1"}`, "space"},
				{`{"q":"a%2Bb","k":"1"}`, "plus"},
			},
		},
		{
			name: "legacy JSON-under-form @",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "ann", body: `{"email":"ann@x.io"}`, legacy: true},
				{name: "bob", body: `{"email":"bob@x.io"}`, legacy: true},
			},
			lives: []livePair{
				{`{"email":"ann%40x.io"}`, "ann"},
				{`{"email":"bob%40x.io"}`, "bob"},
			},
		},
		// Mixed legacy (RawFormBody=false) + newly re-recorded (RawFormBody=true)
		// sets at the same exact-CT endpoint.
		{
			name: "mixed legacy + raw OAuth redirect_uri",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "s1-new", body: "state=s1&redirect_uri=https%3A%2F%2Fa.io%2Fcb", legacy: false},
				{name: "s2-old", body: "state=s2&redirect_uri=https://a.io/cb", legacy: true},
			},
			lives: []livePair{
				{"state=s1&redirect_uri=https%3A%2F%2Fa.io%2Fcb", "s1-new"},
				{"state=s2&redirect_uri=https%3A%2F%2Fa.io%2Fcb", "s2-old"},
			},
		},
		{
			name: "mixed legacy + raw email and note",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "ann-new", body: "email=ann%40example.com&plan=pro&note=a+b+c", legacy: false},
				{name: "bob-old", body: "email=bob@example.com&plan=pro&note=a b c", legacy: true},
			},
			lives: []livePair{
				{"email=ann%40example.com&plan=pro&note=a+b+c", "ann-new"},
				{"email=bob%40example.com&plan=pro&note=a+b+c", "bob-old"},
			},
		},
		{
			name: "mixed legacy + raw timestamp",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "d1-new", body: "id=7&at=2026-11-28T10%3A00%3A00%2B05%3A30", legacy: false},
				{name: "d2-old", body: "id=8&at=2026-11-28T10:00:00+05:30", legacy: true},
			},
			lives: []livePair{
				{"id=7&at=2026-11-28T10%3A00%3A00%2B05%3A30", "d1-new"},
				{"id=8&at=2026-11-28T10%3A00%3A00%2B05%3A30", "d2-old"},
			},
		},
		{
			name: "mixed legacy plus vs new space",
			cts:  []string{exactCT},
			recs: []recSpec{
				{name: "plus-old", body: "q=a+b&k=1", legacy: true},
				{name: "space-new", body: "q=a+b&k=1", legacy: false},
			},
			lives: []livePair{
				{"q=a%2Bb&k=1", "plus-old"},
				{"q=a+b&k=1", "space-new"},
			},
		},
		// New raw recordings (RawFormBody=true on exact CT, false on charset CT):
		// decoded primary scoring + raw-vs-raw tie-breaking pairs every case in
		// all 4 orders, including encoded delimiters and JSON under form CT.
		{
			name: "new raw @",
			cts:  []string{exactCT, charsetCT},
			recs: []recSpec{
				{name: "ann", body: "email=ann%40example.com&plan=pro"},
				{name: "bob", body: "email=bob%40example.com&plan=pro"},
			},
			lives: []livePair{
				{"email=ann%40example.com&plan=pro", "ann"},
				{"email=bob%40example.com&plan=pro", "bob"},
			},
		},
		{
			name: "new raw encoded delimiter a=1%262 vs a=1&2",
			cts:  []string{exactCT, charsetCT},
			recs: []recSpec{
				{name: "enc", body: "a=1%262"},
				{name: "amp", body: "a=1&2"},
			},
			lives: []livePair{
				{"a=1%262", "enc"},
				{"a=1&2", "amp"},
			},
		},
		{
			name: "new raw encoded delimiter q=a%26b%3Dc vs q=a&b=c",
			cts:  []string{exactCT, charsetCT},
			recs: []recSpec{
				{name: "enc", body: "q=a%26b%3Dc"},
				{name: "split", body: "q=a&b=c"},
			},
			lives: []livePair{
				{"q=a%26b%3Dc", "enc"},
				{"q=a&b=c", "split"},
			},
		},
		{
			name: "new raw encoded slash x=a%2Fb vs x=a&y=b",
			cts:  []string{exactCT, charsetCT},
			recs: []recSpec{
				{name: "enc", body: "x=a%2Fb"},
				{name: "split", body: "x=a&y=b"},
			},
			lives: []livePair{
				{"x=a%2Fb", "enc"},
				{"x=a&y=b", "split"},
			},
		},
		{
			name: "new raw plus/space q=a+b&k=1 vs q=a%2Bb&k=1",
			cts:  []string{exactCT, charsetCT},
			recs: []recSpec{
				{name: "space", body: "q=a+b&k=1"},
				{name: "plus", body: "q=a%2Bb&k=1"},
			},
			lives: []livePair{
				{"q=a+b&k=1", "space"},
				{"q=a%2Bb&k=1", "plus"},
			},
		},
		{
			name: "new raw space encoding tie q=a+b&k=1 vs q=a%20b&k=1",
			cts:  []string{exactCT, charsetCT},
			recs: []recSpec{
				{name: "plus-space", body: "q=a+b&k=1"},
				{name: "pct20-space", body: "q=a%20b&k=1"},
			},
			lives: []livePair{
				{"q=a+b&k=1", "plus-space"},
				{"q=a%20b&k=1", "pct20-space"},
			},
		},
		{
			name: "new raw %20 vs + distinct ids",
			cts:  []string{exactCT, charsetCT},
			recs: []recSpec{
				{name: "one", body: "q=a%20b&id=1"},
				{name: "two", body: "q=a+b&id=2"},
			},
			lives: []livePair{
				{"q=a%20b&id=1", "one"},
				{"q=a+b&id=2", "two"},
			},
		},
		{
			name: "new raw JSON under form CT with percent escapes",
			cts:  []string{exactCT, charsetCT},
			recs: []recSpec{
				{name: "lit", body: `{"q":"a&b=c","k":"1"}`},
				{name: "enc", body: `{"q":"a%26b%3Dc","k":"1"}`},
			},
			lives: []livePair{
				{`{"q":"a&b=c","k":"1"}`, "lit"},
				{`{"q":"a%26b%3Dc","k":"1"}`, "enc"},
			},
		},
		{
			name: "new raw three candidates enc, split, other",
			cts:  []string{exactCT, charsetCT},
			recs: []recSpec{
				{name: "enc", body: "q=a%26b&n=1"},
				{name: "split", body: "q=a&b=&n=2"},
				{name: "other", body: "q=c&n=3"},
			},
			lives: []livePair{
				{"q=a%26b&n=1", "enc"},
				{"q=a&b=&n=2", "split"},
				{"q=c&n=3", "other"},
			},
		},
	}

	orders := map[bool]string{false: "fwd", true: "rev"}
	for _, hdrKey := range []string{"Content-Type", "content-type"} {
		for _, row := range rows {
			for _, ct := range row.cts {
				for _, recRev := range []bool{false, true} {
					for _, liveRev := range []bool{false, true} {
						sub := hdrKey + "/" + ct + "/" + row.name + "/rec=" + orders[recRev] + "/live=" + orders[liveRev]
						t.Run(sub, func(t *testing.T) {
							recs := append([]recSpec(nil), row.recs...)
							if recRev {
								for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
									recs[i], recs[j] = recs[j], recs[i]
								}
							}
							lives := append([]livePair(nil), row.lives...)
							if liveRev {
								for i, j := 0, len(lives)-1; i < j; i, j = i+1, j-1 {
									lives[i], lives[j] = lives[j], lives[i]
								}
							}
							mkReq := func(body string, rawForm bool) *models.HTTPReq {
								return &models.HTTPReq{
									Method:      "POST",
									URL:         "http://api/signup",
									Header:      map[string]string{hdrKey: ct},
									Body:        body,
									RawFormBody: rawForm,
								}
							}
							cases := make([]ClientCase, 0, len(recs))
							for _, rc := range recs {
								cases = append(cases, ClientCase{
									Name: rc.name,
									Req:  mkReq(rc.body, ct == exactCT && !rc.legacy),
								})
							}
							used := make([]bool, len(cases))
							for _, lv := range lives {
								liveReq := mkReq(lv.body, ct == exactCT)
								freshIdx, ok := PairCase(cases, make([]bool, len(cases)), liveReq, true)
								if !ok || cases[freshIdx].Name != lv.want {
									t.Fatalf("fresh PairCase(%q) = (%d %q, %v); want %q", lv.body, freshIdx, cases[freshIdx].Name, ok, lv.want)
								}
								seqIdx, ok := PairCase(cases, used, liveReq, true)
								if !ok || cases[seqIdx].Name != lv.want {
									t.Fatalf("seq PairCase(%q) = (%d %q, %v); want %q", lv.body, seqIdx, cases[seqIdx].Name, ok, lv.want)
								}
								used[seqIdx] = true
							}
						})
					}
				}
			}
		}
	}
}

func TestQueryUnescapePrefixBoundsAndEscapes(t *testing.T) {
	// Boundary test: a %26 escape straddling raw byte index limit-1 and limit.
	raw := strings.Repeat("a", 15) + "%26" + "z+1%3D2"
	full, err := url.QueryUnescape(raw)
	if err != nil {
		t.Fatal(err)
	}
	for limit := 0; limit <= len(full)+2; limit++ {
		want := full
		if len(want) > limit {
			want = want[:limit]
		}
		if got := queryUnescapePrefix(raw, limit); got != want {
			t.Fatalf("queryUnescapePrefix(limit=%d) = %q; want %q", limit, got, want)
		}
	}

	// Fast path with no escapes returns exact bounded slice.
	plain := "abcdef123456"
	if got := queryUnescapePrefix(plain, 6); got != "abcdef" {
		t.Fatalf("plain bounded = %q; want abcdef", got)
	}

	// Invalid escape within prefix falls back to raw prefix.
	for _, bad := range []string{"a=50%&b=1", "a=1%2", "a=%zz&b=1"} {
		if got := queryUnescapePrefix(bad, 64); got != bad {
			t.Fatalf("invalid escape %q = %q; want raw fallback", bad, got)
		}
	}
}

func TestPairCaseMixedPrimaryTiePreservesRecordingOrder(t *testing.T) {
	const exactCT = "application/x-www-form-urlencoded"
	for _, hdrKey := range []string{"Content-Type", "content-type"} {
		mkReq := func(body string, rawForm bool) *models.HTTPReq {
			return &models.HTTPReq{
				Method:      "POST",
				URL:         "http://api/signup",
				Header:      map[string]string{hdrKey: exactCT},
				Body:        body,
				RawFormBody: rawForm,
			}
		}

		// Mixed set differing only in run-made values (clock/nonce): legacy
		// recorded first, re-recorded second. Both tie on primary decoded
		// words; recording order must decide, never ranking the marked case
		// ahead of the legacy case by shared raw words.
		t.Run(hdrKey+"/mixed run-made values tie goes by recording order", func(t *testing.T) {
			cases := []ClientCase{
				{Name: "old-1", Req: mkReq("n=111&op=x", false)},
				{Name: "new-2", Req: mkReq("n=222&op=x", true)},
			}
			used := make([]bool, len(cases))
			live1 := mkReq("n=333&op=x", true)
			live2 := mkReq("n=444&op=x", true)
			if i, ok := PairCase(cases, make([]bool, len(cases)), live1, true); !ok || cases[i].Name != "old-1" {
				t.Fatalf("fresh live1 = (%d %q, %v); want old-1", i, cases[i].Name, ok)
			}
			i1, ok1 := PairCase(cases, used, live1, true)
			if !ok1 || cases[i1].Name != "old-1" {
				t.Fatalf("seq live1 = (%d %q, %v); want old-1", i1, cases[i1].Name, ok1)
			}
			used[i1] = true
			i2, ok2 := PairCase(cases, used, live2, true)
			if !ok2 || cases[i2].Name != "new-2" {
				t.Fatalf("seq live2 = (%d %q, %v); want new-2", i2, cases[i2].Name, ok2)
			}
		})

		// Mixed set with escaped field + run-made values: legacy holds decoded
		// '@', re-recorded holds raw '%40'. Both tie on decoded words;
		// recording order must decide so re-recorded never steals live1 by raw
		// '%40' words that the legacy recording no longer holds.
		t.Run(hdrKey+"/mixed escaped run-made values tie goes by recording order", func(t *testing.T) {
			cases := []ClientCase{
				{Name: "old-1", Req: mkReq("email=ann@example.com&n=111", false)},
				{Name: "new-2", Req: mkReq("email=ann%40example.com&n=222", true)},
			}
			used := make([]bool, len(cases))
			live1 := mkReq("email=ann%40example.com&n=333", true)
			live2 := mkReq("email=ann%40example.com&n=444", true)
			if i, ok := PairCase(cases, make([]bool, len(cases)), live1, true); !ok || cases[i].Name != "old-1" {
				t.Fatalf("fresh live1 = (%d %q, %v); want old-1", i, cases[i].Name, ok)
			}
			i1, ok1 := PairCase(cases, used, live1, true)
			if !ok1 || cases[i1].Name != "old-1" {
				t.Fatalf("seq live1 = (%d %q, %v); want old-1", i1, cases[i1].Name, ok1)
			}
			used[i1] = true
			i2, ok2 := PairCase(cases, used, live2, true)
			if !ok2 || cases[i2].Name != "new-2" {
				t.Fatalf("seq live2 = (%d %q, %v); want new-2", i2, cases[i2].Name, ok2)
			}
		})

		// Mixed legacy "a=1&2" + single re-recorded "a=1%262": both decode to
		// "a=1&2" and tie on primary score. Because the legacy recording is
		// never eliminated by raw words it no longer holds, recording order
		// decides for both fresh live "a=1%262" and fresh live "a=1&2".
		t.Run(hdrKey+"/mixed legacy amp + new enc preserves recording order", func(t *testing.T) {
			cases := []ClientCase{
				{Name: "amp-old", Req: mkReq("a=1&2", false)},
				{Name: "enc-new", Req: mkReq("a=1%262", true)},
			}
			used := make([]bool, len(cases))
			liveAmp := mkReq("a=1&2", true)
			liveEnc := mkReq("a=1%262", true)
			if i, ok := PairCase(cases, make([]bool, len(cases)), liveEnc, true); !ok || cases[i].Name != "amp-old" {
				t.Fatalf("fresh liveEnc = (%d %q, %v); want amp-old (recording order)", i, cases[i].Name, ok)
			}
			if i, ok := PairCase(cases, make([]bool, len(cases)), liveAmp, true); !ok || cases[i].Name != "amp-old" {
				t.Fatalf("fresh liveAmp = (%d %q, %v); want amp-old", i, cases[i].Name, ok)
			}
			i1, ok1 := PairCase(cases, used, liveAmp, true)
			if !ok1 || cases[i1].Name != "amp-old" {
				t.Fatalf("seq liveAmp = (%d %q, %v); want amp-old", i1, cases[i1].Name, ok1)
			}
			used[i1] = true
			i2, ok2 := PairCase(cases, used, liveEnc, true)
			if !ok2 || cases[i2].Name != "enc-new" {
				t.Fatalf("seq liveEnc = (%d %q, %v); want enc-new", i2, cases[i2].Name, ok2)
			}
		})

		// Mixed 3-candidate tie with two marked candidates + one legacy
		// candidate: raw-vs-raw still eliminates the strictly worse marked
		// candidate (enc-new on live "a=1&2"), while never eliminating an
		// unmarked legacy candidate ahead in recording order.
		t.Run(hdrKey+"/mixed two marked + legacy eliminates worse marked without dropping legacy", func(t *testing.T) {
			liveAmp := mkReq("a=1&2", true)
			liveEnc := mkReq("a=1%262", true)

			// Marked candidates before legacy: enc-new is dropped on live "a=1&2",
			// so amp-new wins instead of enc-new.
			markedFirst := []ClientCase{
				{Name: "enc-new", Req: mkReq("a=1%262", true)},
				{Name: "amp-new", Req: mkReq("a=1&2", true)},
				{Name: "amp-old", Req: mkReq("a=1&2", false)},
			}
			if i, ok := PairCase(markedFirst, make([]bool, len(markedFirst)), liveAmp, true); !ok || markedFirst[i].Name != "amp-new" {
				t.Fatalf("markedFirst liveAmp = (%d %q, %v); want amp-new", i, markedFirst[i].Name, ok)
			}
			if i, ok := PairCase(markedFirst, make([]bool, len(markedFirst)), liveEnc, true); !ok || markedFirst[i].Name != "enc-new" {
				t.Fatalf("markedFirst liveEnc = (%d %q, %v); want enc-new", i, markedFirst[i].Name, ok)
			}

			// Legacy first before two marked candidates: legacy is never dropped
			// by raw-vs-raw, so recording order still picks amp-old.
			legacyFirst := []ClientCase{
				{Name: "amp-old", Req: mkReq("a=1&2", false)},
				{Name: "enc-new", Req: mkReq("a=1%262", true)},
				{Name: "amp-new", Req: mkReq("a=1&2", true)},
			}
			if i, ok := PairCase(legacyFirst, make([]bool, len(legacyFirst)), liveAmp, true); !ok || legacyFirst[i].Name != "amp-old" {
				t.Fatalf("legacyFirst liveAmp = (%d %q, %v); want amp-old", i, legacyFirst[i].Name, ok)
			}
			if i, ok := PairCase(legacyFirst, make([]bool, len(legacyFirst)), liveEnc, true); !ok || legacyFirst[i].Name != "amp-old" {
				t.Fatalf("legacyFirst liveEnc = (%d %q, %v); want amp-old", i, legacyFirst[i].Name, ok)
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
			if !strings.Contains(d.Line, "observed a value keploy cannot bind at its source: test case post-oauth-par-2 body") ||
				!strings.Contains(d.Line, " holds it, but keploy cannot bind it there (") ||
				strings.Contains(d.Line, "bound at its source") || strings.Contains(d.Line, "bound to ") {
				t.Fatalf("line=%q; want 'observed a value keploy cannot bind at its source: <place> holds it, but keploy cannot bind it there (<reason>)' without contradictory bound wording", d.Line)
			}
			if !strings.Contains(d.Line, tc.wantWhy) || !strings.Contains(d.From.UnbindableWhy, tc.wantWhy) {
				t.Fatalf("line=%q unbindableWhy=%q; want substring %q", d.Line, d.From.UnbindableWhy, tc.wantWhy)
			}
		})
	}
}
