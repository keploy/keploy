package variables

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
)

func TestInRequestFormURLEncoded(t *testing.T) {
	req := &models.HTTPReq{
		Method: "POST",
		URL:    "http://localhost:8083/oauth/par",
		Header: map[string]string{"content-type": "Application/X-WWW-Form-Urlencoded; charset=UTF-8"},
		Body:   "client_id=demo-cli&installation_id=cli-inst-123&note=x%26y%3Dz&dup=a&dup=b",
	}
	held := InRequest(req)
	for _, tc := range []struct {
		name string
		p    Place
		want bool
	}{
		{"single form field", Place{Part: PartBody, Path: []templates.PathSeg{{Key: "installation_id"}}}, true},
		{"field with encoded & and =", Place{Part: PartBody, Path: []templates.PathSeg{{Key: "note"}}}, true},
		{"no phantom key from encoded &", Place{Part: PartBody, Path: []templates.PathSeg{{Key: "y"}}}, false},
		{"missing form field", Place{Part: PartBody, Path: []templates.PathSeg{{Key: "missing"}}}, false},
		{"repeated form field", Place{Part: PartBody, Path: []templates.PathSeg{{Key: "dup"}}}, false},
		{"nested path", Place{Part: PartBody, Path: []templates.PathSeg{{Key: "a"}, {Key: "b"}}}, false},
		{"index path", Place{Part: PartBody, Path: []templates.PathSeg{{Index: 0, IsIndex: true}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := held(tc.p); got != tc.want {
				t.Fatalf("InRequest(%v) = %v; want %v", tc.p, got, tc.want)
			}
		})
	}

	// JSON body sent under a form Content-Type (e.g. jQuery $.ajax default)
	// must stay JSON rather than being parsed as a junk form key.
	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"application/x-www-form-urlencoded; charset=UTF-8",
	} {
		t.Run("JSON body under "+ct, func(t *testing.T) {
			jsonReq := &models.HTTPReq{
				Method: "POST",
				URL:    "http://localhost:8083/orders",
				Header: map[string]string{"Content-Type": ct},
				Body:   `{"userId":"u-rec-1","qty":2}`,
			}
			in := InRequest(jsonReq)
			if !in(Place{Part: PartBody, Path: []templates.PathSeg{{Key: "userId"}}}) {
				t.Fatalf("InRequest must still hold JSON body.userId under %s", ct)
			}
			if !in(Place{Part: PartBody, Path: []templates.PathSeg{{Key: "qty"}}}) {
				t.Fatalf("InRequest must still hold JSON body.qty under %s", ct)
			}
			if _, ok := ParseFormBody(jsonReq.Body, ct); ok {
				t.Fatalf("ParseFormBody must reject JSON body under %s", ct)
			}
		})
	}

	// Non-form Content-Type does not treat form text as a body place.
	plain := InRequest(&models.HTTPReq{
		Method: "POST",
		URL:    "http://localhost:8083/oauth/par",
		Header: map[string]string{"Content-Type": "text/plain"},
		Body:   "installation_id=cli-inst-123",
	})
	if plain(Place{Part: PartBody, Path: []templates.PathSeg{{Key: "installation_id"}}}) {
		t.Fatalf("text/plain request must not match FormLeafAt")
	}

	// Invalid percent-escape anywhere in raw form fails closed (no unparsed raw values).
	for _, body := range []string{
		"installation_id=%zz",
		"discount=50%&name=a+b%20c",
	} {
		bad := InRequest(&models.HTTPReq{
			Method: "POST",
			URL:    "http://localhost:8083/oauth/par",
			Header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			Body:   body,
		})
		if bad(Place{Part: PartBody, Path: []templates.PathSeg{{Key: "installation_id"}}}) ||
			bad(Place{Part: PartBody, Path: []templates.PathSeg{{Key: "name"}}}) {
			t.Fatalf("invalid form escape %q must return false", body)
		}
	}
}

func TestFormLeafAtAndSpliceFormAt(t *testing.T) {
	body := "client_id=demo-cli&redirect_uri=https%3A%2F%2Fapp.example%2Fcb%3Fstate%3Dabc%26nonce%3D999&note=x%26y%3Dz&code_challenge=abc%2Bdef&pct=50%25+off&name=hello+world&dup=a&dup=b"
	key := func(k string) []templates.PathSeg { return []templates.PathSeg{{Key: k}} }
	leaf := func(b, ct string, p []templates.PathSeg) (string, bool) {
		return FormLeafAt(b, ct, p)
	}

	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"Application/X-WWW-Form-Urlencoded; charset=UTF-8",
	} {
		t.Run(ct, func(t *testing.T) {
			if got, ok := leaf(body, ct, key("redirect_uri")); !ok || got != "https://app.example/cb?state=abc&nonce=999" {
				t.Fatalf("FormLeafAt(redirect_uri) = (%q, %v)", got, ok)
			}
			if _, ok := leaf(body, ct, key("nonce")); ok {
				t.Fatalf("FormLeafAt(nonce) must not see phantom key from encoded %%26")
			}
			if got, ok := leaf(body, ct, key("note")); !ok || got != "x&y=z" {
				t.Fatalf("FormLeafAt(note) = (%q, %v); want x&y=z", got, ok)
			}
			if _, ok := leaf(body, ct, key("y")); ok {
				t.Fatalf("FormLeafAt(y) must not see phantom key from encoded %%26/%%3D")
			}
			if got, ok := leaf(body, ct, key("code_challenge")); !ok || got != "abc+def" {
				t.Fatalf("FormLeafAt(code_challenge) = (%q, %v); want abc+def", got, ok)
			}
			if got, ok := leaf(body, ct, key("pct")); !ok || got != "50% off" {
				t.Fatalf("FormLeafAt(pct) = (%q, %v); want 50%% off", got, ok)
			}
			if got, ok := leaf(body, ct, key("name")); !ok || got != "hello world" {
				t.Fatalf("FormLeafAt(name) = (%q, %v)", got, ok)
			}
			if _, ok := leaf(body, ct, key("dup")); ok {
				t.Fatalf("FormLeafAt(dup) must reject repeated key")
			}
			if _, ok := leaf(body, ct, key("")); ok {
				t.Fatalf("FormLeafAt empty key must return false")
			}
			if _, ok := leaf("discount=50%&name=a+b%20c", ct, key("name")); ok {
				t.Fatalf("FormLeafAt invalid escape must fail closed")
			}
			if _, ok := ParseFormBody("&&", ct); ok {
				t.Fatalf("empty pairs only must return false")
			}
		})
	}

	spliced, ok := SpliceFormAt(body, key("name"), "new value+1&k=v")
	if !ok {
		t.Fatalf("SpliceFormAt(name) failed")
	}
	if got, ok := leaf(spliced, "application/x-www-form-urlencoded", key("name")); !ok || got != "new value+1&k=v" {
		t.Fatalf("round-trip FormLeafAt(spliced, name) = (%q, %v); want new value+1&k=v", got, ok)
	}
	if got, ok := leaf(spliced, "application/x-www-form-urlencoded", key("client_id")); !ok || got != "demo-cli" {
		t.Fatalf("sibling key client_id = (%q, %v); want demo-cli", got, ok)
	}
	if _, ok := SpliceFormAt(`{"name":"a"}`, key("name"), "b"); ok {
		t.Fatalf("SpliceFormAt must reject JSON body")
	}
	if _, ok := SpliceFormAt(body, key("dup"), "x"); ok {
		t.Fatalf("SpliceFormAt(dup) must reject repeated key")
	}
	if _, ok := SpliceFormAt(body, key("missing"), "x"); ok {
		t.Fatalf("SpliceFormAt(missing) must reject missing key")
	}
	if _, ok := SpliceFormAt(body, []templates.PathSeg{{Index: 0, IsIndex: true}}, "x"); ok {
		t.Fatalf("SpliceFormAt(index) must reject index path")
	}
	if _, ok := SpliceFormAt("a=%zz", key("a"), "x"); ok {
		t.Fatalf("SpliceFormAt invalid escape must return false")
	}
	if !IsFormMediaType("application/x-www-form-urlencoded") || !IsFormMediaType("Application/X-WWW-Form-Urlencoded; charset=UTF-8") {
		t.Fatalf("IsFormMediaType must accept form media types with optional params and any case")
	}
	if IsFormMediaType("application/json") || IsFormMediaType("") || IsFormMediaType("not/a/type") {
		t.Fatalf("IsFormMediaType must reject non-form or invalid media types")
	}
}
