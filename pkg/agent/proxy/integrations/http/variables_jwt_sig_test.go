package http

import (
	"encoding/base64"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// sigJWTBody is a store call's body holding a JWT: an HS256 header (alg
// alg), the claims, and sig as its signature's text.
func sigJWTBody(alg, claims, sig string) string {
	enc := base64.RawURLEncoding
	return `{"token":"` + enc.EncodeToString([]byte(`{"alg":"`+alg+`","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(claims)) + "." + sig + `"}`
}

const (
	httpSigRec  = "ssykarGU4Lp7Ktw6QuR9f8fbNsyoQDGqhZGjDJhJUEs"
	httpSigFive = "2-Eg58Sc-AWt1hpQzYRxPg-61F0f2C3G-dO0dsUnz4k"
)

// TestVariables_AJWTSignatureBoundAtItsStepIsReadWhole: a variable bound at
// a token's |jwtsig binds this run's signature whole, of any count of
// keploy's words, and the call is compared with it put back: its header
// and claims as recorded. A changed header, a changed claim, or a
// signature of another form (another length, or no base64url text) is a
// miss: the signature is checked by its form, nothing else in the token
// is loosened.
func TestVariables_AJWTSignatureBoundAtItsStepIsReadWhole(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const claims = `{"sub":"u1","tokenType":"refresh"}`
	mocks := func() []*models.Mock {
		return []*models.Mock{at(varMock("mock-1", "PUT", "http://store/tokens", sigJWTBody("HS256", claims, httpSigRec), 204, `{}`,
			models.MockVariables{"sig": bindV(httpSigRec, "body.token|jwtsig")}), 1)}
	}
	h := newHTTP()
	db := &varDb{perTest: mocks()}
	ok, stub, diag := matchVar(t, h, db, liveReq("PUT", "/tokens", sigJWTBody("HS256", claims, httpSigFive)))
	if !ok || stub.Name != "mock-1" {
		t.Fatalf("the PUT should be answered by mock-1: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
	if b, bound := db.table.Lookup("sig"); !bound || b.Value != httpSigFive || b.Recorded != httpSigRec || b.Place != "body.token|jwtsig" {
		t.Fatalf("sig bound as %+v (%v), want %s at body.token|jwtsig", b, bound, httpSigFive)
	}
	for name, body := range map[string]string{
		"a changed header":         sigJWTBody("HS384", claims, httpSigFive),
		"a changed claim":          sigJWTBody("HS256", `{"sub":"u2","tokenType":"refresh"}`, httpSigFive),
		"a longer signature":       sigJWTBody("HS256", claims, httpSigFive+"AAAA"),
		"no base64url signature":   sigJWTBody("HS256", claims, "2+Eg58Sc/AWt1hpQzYRxPg-61F0f2C3G-dO0dsUnz4k"),
		"a padded signature":       sigJWTBody("HS256", claims, httpSigFive[:42]+"="),
		"no signature, no JWT now": `{"token":"` + httpSigFive + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			db := &varDb{perTest: mocks()}
			if ok, stub, _ := matchVar(t, newHTTP(), db, liveReq("PUT", "/tokens", body)); ok {
				t.Fatalf("%s was answered by %s; want a miss", name, stub.Name)
			}
		})
	}
}
