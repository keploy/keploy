package variables

import (
	"encoding/base64"
	"testing"
)

// sigToken is a JWT: an HS256 header, a payload of claims, and sig as its
// signature's text.
func sigToken(claims, sig string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(claims)) + "." + sig
}

const (
	sigA = "ssykarGU4Lp7Ktw6QuR9f8fbNsyoQDGqhZGjDJhJUEs"
	sigB = "2-Eg58Sc-AWt1hpQzYRxPg-61F0f2C3G-dO0dsUnz4k"
)

// TestJWTSigStepOpensTheSignatureWhole: jwtsig opens a signed JWT's last
// part whole, "Bearer " before it or not, and nothing that is no signed
// JWT: a dotted text whose middle is no JSON object, an unsigned token, a
// padded or standard-alphabet last part.
func TestJWTSigStepOpensTheSignatureWhole(t *testing.T) {
	tok := sigToken(`{"sub":"u1"}`, sigB)
	for _, text := range []string{tok, "Bearer " + tok} {
		if got, ok := Decode(StepJWTSig, text); !ok || got != sigB {
			t.Errorf("Decode(jwtsig, %q) = %q, %v; want %q", text, got, ok, sigB)
		}
	}
	for _, text := range []string{
		"order.v1." + sigB,
		sigToken(`{"sub":"u1"}`, ""),
		sigToken(`{"sub":"u1"}`, sigA+"="),
		sigToken(`{"sub":"u1"}`, "ab+cd/ef"),
		sigToken(`{"sub":"u1"}`, sigA) + ".x",
	} {
		if got, ok := Decode(StepJWTSig, text); ok {
			t.Errorf("Decode(jwtsig, %q) = %q; want no signature", text, got)
		}
	}
	a, err := ParseAddress("body.docs.msgs.0.token|jwtsig")
	if err != nil || len(a.Steps) != 1 || a.Steps[0] != StepJWTSig || a.Word != -1 || a.String() != "body.docs.msgs.0.token|jwtsig" {
		t.Fatalf("ParseAddress = %+v, %v", a, err)
	}
	if got, ok := a.WordOf(tok); !ok || got != sigB {
		t.Errorf("WordOf = %q, %v; want %q", got, ok, sigB)
	}
}

// TestJWTSigStepWritesASignatureOfItsForm: a write at jwtsig replaces the
// signature and keeps every other byte, whatever the two signatures' words;
// one of another length, or not base64url, is refused (a JWT's signature
// is checked by its form only, SignatureForm), and so is a text that is no
// signed JWT.
func TestJWTSigStepWritesASignatureOfItsForm(t *testing.T) {
	at := Address{Steps: []Step{StepJWTSig}, Word: -1}
	tok := sigToken(`{"sub":"u1","exp":1}`, sigA)
	out, ok := at.ReplaceWord("Bearer "+tok, sigB)
	if want := "Bearer " + sigToken(`{"sub":"u1","exp":1}`, sigB); !ok || out != want {
		t.Fatalf("ReplaceWord = %q, %v; want %q", out, ok, want)
	}
	for _, v := range []string{sigB + "AA", sigB[:42], "ab+d" + sigB[4:], sigB[:42] + "=", sigB[:20] + "." + sigB[21:]} {
		if out, ok := at.ReplaceWord(tok, v); ok {
			t.Errorf("ReplaceWord(%q) = %q; want refused", v, out)
		}
	}
	if out, ok := at.ReplaceWord("order.v1."+sigA, sigB); ok {
		t.Errorf("ReplaceWord on no JWT = %q; want refused", out)
	}
	if !SignatureForm(sigA, sigB) || SignatureForm(sigA, sigB[:42]) || SignatureForm("ab+d"+sigB[4:], sigB) || SignatureForm(sigB, "ab+d"+sigB[4:]) || SignatureForm("", "") {
		t.Error("SignatureForm: want one length of base64url text, non-empty, in both")
	}
}

// TestJWTSigStepStandsInTheTokensLastPart: a write at the signature moves
// no word of the header before it, so the two are written in either
// order; a claim's text is the token whole (its write re-encodes the
// payload), so it overlaps the signature (fail closed).
func TestJWTSigStepStandsInTheTokensLastPart(t *testing.T) {
	tok := "Bearer " + sigToken(`{"sub":"u1"}`, sigA)
	sig := Address{Steps: []Step{StepJWTSig}, Word: -1}
	first := Address{Word: 0}
	if sig.Overlaps(first, tok) || first.Overlaps(sig, tok) {
		t.Error("the signature overlaps the leaf's first word")
	}
	if claim := (Address{Steps: []Step{JWTClaim("sub")}, Word: -1}); !sig.Overlaps(claim, tok) || !claim.Overlaps(sig, tok) {
		t.Error("the signature does not overlap a claim")
	}
	out, i, j, ok := PutWords(tok, []WordPut{{At: first, From: "Bearer", To: "Token"}, {At: sig, From: sigA, To: sigB}})
	if want := "Token " + sigToken(`{"sub":"u1"}`, sigB); !ok || out != want {
		t.Fatalf("PutWords = %q (%d, %d, %v); want %q", out, i, j, ok, want)
	}
}
