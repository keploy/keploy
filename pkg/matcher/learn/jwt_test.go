package learn

import (
	"encoding/base64"
	"reflect"
	"testing"
)

// testJWT is a compact JWT of a JOSE header and claims (JSON text) and a
// signature segment.
func testJWT(header, claims, sig string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(header)) + "." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + "." + sig
}

func TestReadJWTReadsATokenByWhatItSays(t *testing.T) {
	tok := testJWT(`{"typ":"JWT","alg":"HS256"}`, `{"sub":"u-1","role":"admin","exp":1793374115,"ctx":{"cid":"c-1"}}`, "c2lnbmF0dXJlLW9uZQ")
	j, ok := ReadJWT("Bearer " + tok)
	if !ok {
		t.Fatal("a Bearer token is a JWT")
	}
	if j.Scheme != "Bearer " || j.Header != `{"alg":"HS256","typ":"JWT"}` || j.Signature != "c2lnbmF0dXJlLW9uZQ" {
		t.Fatalf("read %+v", j)
	}
	if j.SignatureForm() != "base64url, 18 characters" {
		t.Fatalf("signature form %q", j.SignatureForm())
	}
	leaves := map[string]Leaf{}
	j.Leaves("header.authorization", leaves)
	want := map[string]string{
		"header.authorization{jwt}.sub":     "u-1",
		"header.authorization{jwt}.role":    "admin",
		"header.authorization{jwt}.exp":     "1793374115",
		"header.authorization{jwt}.ctx.cid": "c-1",
	}
	got := map[string]string{}
	for p, l := range leaves {
		got[p] = l.Value
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("claims %v, want %v", got, want)
	}
	if leaves["header.authorization{jwt}.exp"].Type != Number {
		t.Fatal("a number claim is a number leaf")
	}
	if c, ok := ClaimOf("header.authorization{jwt}.ctx.cid"); !ok || c != "header.authorization" {
		t.Fatalf("ClaimOf %q %v", c, ok)
	}
	if _, ok := ClaimOf("header.authorization"); ok {
		t.Fatal("a header is no claim")
	}
	if alone, ok := ReadJWT(tok); !ok || alone.Scheme != "" {
		t.Fatalf("a token alone is one too: %+v %v", alone, ok)
	}

	for name, s := range map[string]string{
		"two segments":            "Bearer " + tok[:len(tok)-len(".c2lnbmF0dXJlLW9uZQ")],
		"a header naming no alg":  testJWT(`{"typ":"JWT"}`, `{"sub":"u-1"}`, "c2ln"),
		"no signature, signed":    testJWT(`{"alg":"HS256"}`, `{"sub":"u-1"}`, ""),
		"claims that are no JSON": testJWT(`{"alg":"HS256"}`, `sub=u-1`, "c2ln"),
		"claims that are a list":  testJWT(`{"alg":"HS256"}`, `["u-1"]`, "c2ln"),
		"padded base64":           testJWT(`{"alg":"HS256"}`, `{"sub":"u-1"}`, "c2ln") + "=",
		"a scheme of two words":   "Bearer token " + tok,
		"a scheme with a digit":   "B3arer " + tok,
		"an opaque token":         "Bearer kep_e2eGoSuiteTestOnlyKey_notASecret",
		"a dotted name":           "api.example.com",
	} {
		if _, ok := ReadJWT(s); ok {
			t.Errorf("%s: read as a JWT", name)
		}
	}
	if j, ok := ReadJWT(testJWT(`{"alg":"none"}`, `{"sub":"u-1"}`, "")); !ok || j.SignatureForm() != "base64url, 0 characters" {
		t.Fatalf("an unsecured token is one, with no signature: %+v %v", j, ok)
	}
}
