package learn

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/variables"
)

// sigJWT is a refresh token as api-server signs it at a CLI login, its
// signature's base64url text sig: random bytes, so a '-' or '_' in it, and
// so its count of keploy's words, changes from run to run.
func sigJWT(read time.Time, sid, jti, sig string) string {
	return sigJWTWith(`{"alg":"HS256","typ":"JWT"}`, read, sid, jti, sig)
}

// sigJWTWith is sigJWT under the header header.
func sigJWTWith(header string, read time.Time, sid, jti, sig string) string {
	enc := base64.RawURLEncoding
	payload := fmt.Sprintf(`{"userId":"68d4148edcd121253694486c","tokenType":"refresh","auth_time":%d,"sid":"%s","exp":%d,"jti":"%s"}`,
		read.Unix(), sid, read.Add(30*24*time.Hour).Unix(), jti)
	return enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString([]byte(payload)) + "." + sig
}

// The recorded signature, one of keploy's words, and round F's: of one
// word, of two ('-'), of one with a leading '_', and of five.
const (
	sigRec      = "ssykarGU4Lp7Ktw6QuR9f8fbNsyoQDGqhZGjDJhJUEs"
	sigTwoWords = "vWyN5YYptqCYhsLsN8P3-41Rrq8a4ycfLtS541165V4"
	sigOneWord  = "vWyN5YYptqCYhsLsN8P3x41Rrq8a4ycfLtS541165V4"
	sigLeading  = "_HiALUw1VuleNalvjkEYLlrFYI403ci2ZVOfJfDqQOU"
	sigFive     = "2-Eg58Sc-AWt1hpQzYRxPg-61F0f2C3G-dO0dsUnz4k"
)

const (
	sigSidR, sigJtiR = "2890b95a-120f-42c1-9674-11047dca8877", "72d43fe7-022c-4f5b-9c6b-d68ad0f47aa1"
	sigSidF, sigJtiF = "c946d4c2-5349-4955-9cfe-89c73e3a98e0", "8282c3f2-5746-489e-af3c-5bf259cce89d"
)

// sigCall is the insert of a refresh token at p: recorded with the
// signature sigRec, seen in round F with fSig (under round F's header
// fHeader), the claims' times read from the call's clock under the time
// default, which checks a time in a claim by its form (Caps.ClaimByForm, as
// Mongo's adapter reads one).
func sigCall(p, fHeader, fSig string, earlier []Earlier) (Input, time.Time, time.Time) {
	rAt, fAt := oAt.Add(500*time.Millisecond), oAt.Add(10*time.Hour+500*time.Millisecond)
	rec := sigJWT(rAt, sigSidR, sigJtiR, sigRec)
	live := sigJWTWith(fHeader, fAt, sigSidF, sigJtiF, fSig)
	w := iWin(rAt, fAt, 30*time.Millisecond)
	w.offsets = true
	in := iCall(p, String, rec, live, rAt, fAt, w, earlier, nil)
	in.Drifts.clientTests = true
	in.TimeForm, in.Caps.ClaimByForm = true, true
	return in, rAt, fAt
}

const hs256 = `{"alg":"HS256","typ":"JWT"}`

// TestAJWTSignatureIsDecidedWholeAtItsStep (gl5 p1/p2/p3: mock-870
// body.docs.msgs.0.token refused changed-shape, "a word was added or
// dropped"): a JWT's signature is checked by its form only, so it is one
// value at the token's |jwtsig, made anew at the call, whatever its count
// of words in either run; no decision reads a word of it, and none
// refuses the field.
func TestAJWTSignatureIsDecidedWholeAtItsStep(t *testing.T) {
	const p = "body.docs.msgs.0.token"
	for _, fSig := range []string{sigTwoWords, sigOneWord, sigLeading, sigFive} {
		t.Run(fSig, func(t *testing.T) {
			in, _, _ := sigCall(p, hs256, fSig, nil)
			ds := Decide(in)
			var sig *Decision
			for i, d := range ds {
				if d.Refusal != nil {
					t.Errorf("refused: %s", d.Refusal.Why)
				}
				if strings.Contains(d.Recorded, "ssykar") && d.Recorded != sigRec {
					t.Errorf("a decision reads part of the signature: %s", oDescribe(ds[i:i+1]))
				}
				if d.Recorded == sigRec {
					sig = &ds[i]
				}
			}
			if sig == nil {
				t.Fatalf("no decision on the signature in %s", oDescribe(ds))
			}
			want := variables.Address{Place: sig.At.Place, Steps: []variables.Step{variables.StepJWTSig}, Word: -1}
			if sig.At.String() != want.String() || sig.Full != fSig || sig.Outcome != OutcomeFresh {
				t.Errorf("signature decided %s, want %s made anew to %s", oDescribe([]Decision{*sig}), want, fSig)
			}
		})
	}
}

// TestAJWTSignatureAnEarlierCallSentIsBoundAtItsStep: a call stores a
// token's signature alone (a session key), after the call before it sent
// the token as its bearer (a header: this Input names a body leaf no
// place, as a shape with no Body does). The signature is bound to that call at its |jwtsig, never at
// a word of the token: a replay would read another word there where round
// F's signature, or its payload, holds another count of words.
func TestAJWTSignatureAnEarlierCallSentIsBoundAtItsStep(t *testing.T) {
	const p, from = "body.docs.msgs.0.key", "header.Authorization"
	for _, fSig := range []string{sigOneWord, sigTwoWords, sigFive} {
		t.Run(fSig, func(t *testing.T) {
			tok, rAt, fAt := sigCall(from, hs256, fSig, nil)
			rec, live := tok.Rec.Leaves[from].Value, tok.Live.Leaves[from].Value
			sent := iSent("mock-870", rAt.Add(-5*time.Millisecond), from, String, "Bearer "+rec, "Bearer "+live)
			in := iCall(p, String, sigRec, fSig, rAt, fAt, iWin(rAt, fAt, 30*time.Millisecond), []Earlier{sent}, nil)
			d := oWord(t, Decide(in), sigRec, fSig)
			if d.Outcome != OutcomeValue || d.From.Mock != "mock-870" {
				t.Fatalf("signature decided %s, want bound to mock-870", oDescribe([]Decision{d}))
			}
			if got, want := d.FromAt.String(), from+"|jwtsig"; got != want {
				t.Errorf("bound at %s, want %s", got, want)
			}
		})
	}
}

// TestAJWTSignatureOfAnotherFormIsRefused: false-green guard. A signature
// of another length (the header's algorithm changed with it) is refused,
// the field whole, as a changed shape. A text that is no JWT (a dotted id
// whose last part gains a word) keeps today's reading: no signature step,
// refused as a changed shape.
func TestAJWTSignatureOfAnotherFormIsRefused(t *testing.T) {
	const p = "body.docs.msgs.0.token"
	t.Run("another length", func(t *testing.T) {
		in, _, _ := sigCall(p, hs256, sigOneWord+"AAAA", nil)
		if !oRefusedShape(Decide(in), "its JWT's signature changed form") {
			t.Errorf("a signature of another length is not refused: %s", oDescribe(Decide(in)))
		}
	})
	t.Run("no JWT", func(t *testing.T) {
		rAt, fAt := oAt.Add(500*time.Millisecond), oAt.Add(10*time.Hour+500*time.Millisecond)
		in := iCall(p, String, "order.v1.ssykarGU4Lp7Ktw6QuR9f8fbNsyoQDGqhZGjDJhJUEs", "order.v1.vWyN5YYptqCYhsLsN8P3-41Rrq8a4ycfLtS541165V4", rAt, fAt, iWin(rAt, fAt, 30*time.Millisecond), nil, nil)
		ds := Decide(in)
		if !oRefusedShape(ds, "a word was added or dropped") {
			t.Errorf("a dotted text that is no JWT is not refused as before: %s", oDescribe(ds))
		}
		for _, d := range ds {
			if strings.Contains(d.At.String(), "jwtsig") {
				t.Errorf("a text that is no JWT is read at a signature step: %s", oDescribe(ds))
			}
		}
	})
}

// oRefusedShape reports whether ds refuses its field as a changed shape,
// the refusal's reason holding why.
func oRefusedShape(ds []Decision, why string) bool {
	for _, d := range ds {
		if d.Refusal != nil && d.Refusal.Kind == RefusalChangedShape && strings.Contains(d.Refusal.Why, why) {
			return true
		}
	}
	return false
}
