package learn

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// A JWT the application signs and sends in a call (a header, a body field)
// holds times in its claims: when it was issued, when it expires. Under the
// time default (keploy.yml's times setting at its default) a time is checked
// by its type and form alone, and one inside a claim is no exception: a
// claim that changed between the two record runs, and is a time in both
// (claimTime), is learned as checked by its form at that claim
// (models.ClockOffset.Claim, keyed ClaimClockKey), as MongoDB's learning
// learns one. Everything else in the token stays exact: its header and every
// other claim byte for byte, and its signature by its form alone
// (variables.SignatureForm: the founder's 2026-10-31 ruling), since it
// changes with any claim. A token one of whose other claims changed is left
// as learned. mock.times: strict learns none of it.

// numericDates are the claims the JWT specifications type a NumericDate, a
// time in seconds since the epoch: RFC 7519's exp, nbf and iat (section
// 4.1) and OpenID Connect Core's auth_time (section 2). A JSON number in one
// is a time by its token's own specification, as a decoder-typed time is by
// its protocol's; a claim of any other name is a time only where learning
// read it as the application's clock (claimTime).
var numericDates = map[string]bool{"exp": true, "nbf": true, "iat": true, "auth_time": true}

// ClaimClockKey is the key (models.MockSpec.Clocks) of a time learned in
// the claim claim of the JWT the leaf at path holds: the path, then the
// claim's step, as a word inside a leaf is addressed and as MongoDB's
// learning keys one. A keploy that does not read it finds no leaf at the
// key, so it compares the token as recorded: its times changed, and it is
// refused (fail closed).
func ClaimClockKey(path, claim string) string {
	return path + "|" + string(variables.JWTClaim(claim))
}

// ClaimClockField is the leaf path a clock read k, o (a key of
// models.MockSpec.Clocks and its offset) checks a claim of the JWT at: ok
// only where o is a time in a claim (models.ClockOffset.Claim) and k is
// that claim's key (ClaimClockKey). A key of a claim with an offset of
// none, or of another claim, is no such read.
func ClaimClockField(k string, o models.ClockOffset) (string, bool) {
	if o.Claim == "" {
		return "", false
	}
	field, ok := strings.CutSuffix(k, "|"+string(variables.JWTClaim(o.Claim)))
	return field, ok && field != "" && !strings.Contains(field, "|")
}

// ClaimTimesLearned is ln, a drifted leaf the classify path learned (the
// HTTP adapter's learn step), as the times in the claims of the JWT it
// holds, under the time default (timeForm), in w, its call's window: one
// KindClockRead per claim that changed, at ClaimClockKey of its leaf and
// claim, checked by its form alone (HoldsClaimsByForm). ok only where ln is
// learned noise, or refused only as an instant its call's window shows no
// read of the clock in (unreadHeld: the classify path reads a token as its
// words, and a claim's time is no word of it), a text at a header or a body
// field whose path holds no '|', a JWT in both runs, of one set of claims,
// whose every claim that changed is a time in both runs (claimTime), and
// that, with each such claim's recorded value written back, is the
// recorded token but for its signature, which is of the recorded one's
// form. Never for a variable, an
// echo, a clock read, any other refusal, a token any other claim of which
// changed (it stays as learned), or with the time default off.
func ClaimTimesLearned(ln Learned, w *Window, timeForm bool) ([]Learned, bool) {
	switch {
	case !timeForm:
		return nil, false
	case ln.Kind == KindNoise:
	case ln.Kind == KindUnaccounted && unreadHeld(ln) && ln.sourceWhy == "" && !ln.byOffset:
	default:
		return nil, false
	}
	r, l := ln.Recorded, ln.Live
	if r.Type != String || l.Type != String || !strings.HasPrefix(r.Path, "body.") && !strings.HasPrefix(r.Path, "header.") {
		return nil, false
	}
	if strings.Contains(r.Path, "|") {
		// A field whose name holds a '|': a claim's key of it would read as
		// another kind of key (ClaimClockField), so replay could not find
		// the times learned in it.
		return nil, false
	}
	_, rc, okR := jwtClaims(r.Value)
	_, lc, okL := jwtClaims(l.Value)
	if !okR || !okL {
		return nil, false
	}
	// A claim round F's token lacks is no time in it (claimTime), and one
	// it adds is no part of the recorded token (HoldsClaimsByForm, below).
	var changed []string
	for c, rv := range rc {
		switch {
		case sameJSON(rv, lc[c]):
		case strings.ContainsAny(c, "|#\\"):
			// A claim whose name a key cannot name alone.
			return nil, false
		default:
			changed = append(changed, c)
		}
	}
	if len(changed) == 0 {
		// The claims are the recorded ones (its signature alone changed):
		// no time to check by form.
		return nil, false
	}
	sort.Strings(changed)
	out := make([]Learned, 0, len(changed))
	offs := make([]models.ClockOffset, 0, len(changed))
	for _, c := range changed {
		rl, ll, why := claimTime(c, rc[c], lc[c], w)
		if why == "" {
			return nil, false
		}
		form, formF := models.FormsOf(TimeForm(rl), TimeForm(ll))
		o := models.ClockOffset{Form: form, FormF: formF, Claim: c}
		o.Why = r.Path + " holds a JWT whose " + c + " claim is a time (" + why + "), checked by its form alone (" + o.Forms() +
			", keploy.yml's times setting at its default); the token's header and every other claim are checked as recorded, its signature by its form"
		offs = append(offs, o)
		rl.Path, ll.Path = ClaimClockKey(r.Path, c), ClaimClockKey(r.Path, c)
		read := o
		out = append(out, Learned{Kind: KindClockRead, Recorded: rl, Live: ll, Read: &read})
	}
	if ok, _ := HoldsClaimsByForm(offs, r.Value, l.Value, true); !ok {
		return nil, false
	}
	return out, true
}

// claimTime is the claim c's value as recorded (rv) and in round F (lv) as
// leaves, and why it is a time in both runs: "" where it is none. A time
// here is a JSON number in a claim the JWT specifications type a
// NumericDate (numericDates) whose moment lies near its call in each run
// (epochNearIn), or a value that is, in each run, a read of the
// application's clock in that run's span of the call (readInSpan). Never by
// its shape alone: a claim of another name whose value reads as an instant
// learning did not read in its call's window is no time here.
func claimTime(c string, rv, lv json.RawMessage, w *Window) (rl, ll Leaf, why string) {
	var ok bool
	if rl, ok = claimLeaf(rv); !ok {
		return Leaf{}, Leaf{}, ""
	}
	if ll, ok = claimLeaf(lv); !ok {
		return Leaf{}, Leaf{}, ""
	}
	if TimeForm(rl) == "" || TimeForm(ll) == "" || !epochNearIn(rl, w, false, nil) || !epochNearIn(ll, w, true, nil) {
		return Leaf{}, Leaf{}, ""
	}
	switch {
	case numericDates[c] && rl.Type == Number && ll.Type == Number:
		return rl, ll, "a NumericDate the JWT specifications define"
	case w != nil && readInSpan(rl, w.Rec) && readInSpan(ll, w.F):
		return rl, ll, "a read of the application's clock in its call's window in both runs"
	}
	return Leaf{}, Leaf{}, ""
}

// claimLeaf is raw, a claim's JSON value, as a leaf of its JSON type: a
// Number's or a String's; ok is false for any other value.
func claimLeaf(raw json.RawMessage) (Leaf, bool) {
	v, ok := variables.Decode(variables.StepJSON, string(raw))
	if !ok || v == "" {
		return Leaf{}, false
	}
	if raw[0] == '"' {
		return Leaf{Type: String, Value: v}, true
	}
	return Leaf{Type: Number, Value: v}, true
}

// readInSpan reports whether l names a moment keploy reads as a read of the
// application's clock made in s, a run's span of the call, at its
// resolution. False for a wall clock of no zone, which names no moment, and
// where s is unknown.
func readInSpan(l Leaf, s Span) bool {
	at, res, wall, ok := clockReadOf(l)
	if !ok || wall || s.From.IsZero() {
		return false
	}
	return at.Add(res).After(s.From) && !at.After(s.To)
}

// HoldsClaimsByForm reports whether live, the value a replay's call carries
// at a leaf whose recorded value rec is a JWT with times learned in its
// claims (os, each a models.ClockOffset with its Claim: ClaimTimesLearned),
// passes that check, and why not where it does not: under the time default
// (timeForm) alone, each such claim of live's token a time of the form
// learned (HoldsClaimByForm), and, with each one's recorded value written
// back in its place, live's token rec's byte for byte in its header and its
// payload, and its signature of rec's signature's form
// (variables.SignatureForm). HTTP and MongoDB check such a token alike: a
// changed header, a changed claim that is no time, and a signature of
// another form are refused.
func HoldsClaimsByForm(offs []models.ClockOffset, rec, live string, timeForm bool) (bool, string) {
	if len(offs) == 0 {
		return false, "no time was learned in a claim of the JWT"
	}
	if !timeForm {
		return false, "the times in the claims of its JWT were learned under the time default, checked by their form alone; re-record with mock.times: strict to check their range"
	}
	put := live
	for _, o := range offs {
		if !HoldsClaimByForm(o, live, timeForm) {
			return false, "its JWT's " + o.Claim + " claim was learned as a time written as " + o.Forms() + ", and this call's is " + formWhy(ClaimTimeForm(live, o.Claim)) + ": it changed"
		}
		raw, ok := claimRawOf(rec, o.Claim)
		if !ok {
			return false, "the recorded JWT holds no one " + o.Claim + " claim"
		}
		if put, ok = claimPutBack(put, o.Claim, raw); !ok {
			return false, "this call's JWT holds no one " + o.Claim + " claim"
		}
	}
	rp, lp := strings.Split(rec, "."), strings.Split(put, ".")
	switch {
	case len(rp) != 3 || len(lp) != 3 || rp[0] != lp[0] || rp[1] != lp[1]:
		return false, "its JWT differs from the recorded one outside the times learned in its claims: its header or another claim changed"
	case !variables.SignatureForm(rp[2], lp[2]):
		return false, "its JWT's signature is not of the recorded one's form (base64url of its length)"
	}
	return true, ""
}

// claimRawOf is the raw JSON of the claim claim of token, a JWT: ok is
// false where its payload holds no one such claim (claimSpan).
func claimRawOf(token, claim string) (string, bool) {
	b, _, ok := jwtPayload(token)
	if !ok {
		return "", false
	}
	lo, hi, ok := claimSpan(b, claim)
	if !ok {
		return "", false
	}
	return string(b[lo:hi]), true
}

// claimPutBack is token, a JWT, with raw (a JSON value) written in place of
// its claim claim's value, for a replay's comparison with its recording
// only: its payload re-encoded in the encoding it was written in, padded or
// not, every other byte of the token as it was. ok is false where its
// payload holds no one such claim.
func claimPutBack(token, claim, raw string) (string, bool) {
	b, enc, ok := jwtPayload(token)
	if !ok {
		return "", false
	}
	lo, hi, ok := claimSpan(b, claim)
	if !ok {
		return "", false
	}
	parts := strings.Split(token, ".")
	parts[1] = enc.EncodeToString([]byte(string(b[:lo]) + raw + string(b[hi:])))
	return strings.Join(parts, "."), true
}

// jwtPayload is the decoded payload of token, a JWT of three '.' parts,
// and the base64url encoding its payload is written in; ok is false
// otherwise.
func jwtPayload(token string) ([]byte, *base64.Encoding, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, false
	}
	enc := base64.RawURLEncoding
	if strings.HasSuffix(parts[1], "=") {
		enc = base64.URLEncoding
	}
	b, err := enc.DecodeString(parts[1])
	if err != nil {
		return nil, nil, false
	}
	return b, enc, true
}

// claimSpan is where the value of the claim claim stands in payload, a JSON
// object: its bytes payload[lo:hi]. ok is false where payload is no one
// JSON object, or holds no such member, or holds it more than once.
func claimSpan(payload []byte, claim string) (lo, hi int, ok bool) {
	d := json.NewDecoder(bytes.NewReader(payload))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return 0, 0, false
	}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return 0, 0, false
		}
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return 0, 0, false
		}
		if key, _ := t.(string); key == claim {
			if ok {
				return 0, 0, false
			}
			end := int(d.InputOffset())
			lo, hi, ok = end-len(v), end, true
		}
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') {
		return 0, 0, false
	}
	if _, err := d.Token(); err != io.EOF {
		return 0, 0, false
	}
	return lo, hi, ok
}
