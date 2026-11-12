package http

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// timedLearnDb is learnDb in a run that turned the time default on
// (integrations.TimeFormReader), as keploy.yml's times setting at its
// default says.
type timedLearnDb struct{ *learnDb }

func (timedLearnDb) TimeForm() bool { return true }

// claimJWT is a token of the header head and the claims given, signed sig
// (its third part, as given, unencoded).
func claimJWT(head, claims, sig string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(head)) + "." + enc.EncodeToString([]byte(claims)) + "." + sig
}

const (
	hs256Head = `{"alg":"HS256","typ":"JWT"}`
	// recSig and liveSig are two HS256 signatures: base64url of 32 bytes.
	recSig  = "c2lnbmF0dXJlLWJ5dGVzLXJlY29yZGVkLTAwMDAwMDA"
	liveSig = "c2lnbmF0dXJlLWJ5dGVzLW9mLXRoaXMtcnVuLTExMTE"
)

// notifyClaims are a notify call's token claims: its subject, issued at
// at (iat, epoch seconds), expiring an hour on (exp), and extra, more
// members written after them ("" for none).
func notifyClaims(sub string, at time.Time, extra string) string {
	s := at.Unix()
	return `{"sub":"` + sub + `","iat":` + strconv.FormatInt(s, 10) + `,"exp":` + strconv.FormatInt(s+3600, 10) + extra + `}`
}

// notifyBody and notifyHeader are a notify call carrying token in its body
// and in its X-Api-Token header.
func notifyBody(token string) string { return `{"to":"u-1","token":"` + token + `"}` }

func notifyHeader(token string) map[string]string { return map[string]string{"X-Api-Token": token} }

// learnNotify runs the learn step on a client test's notify call whose
// token (in its body and its X-Api-Token header) the application minted at
// its call, recorded with claims rec and in round F with claims made by
// liveClaims at round F's call, under the time default where timeForm is
// set. It returns the mock the step consumed (nil where it refused the
// call) and its refusal.
func learnNotify(t *testing.T, rec string, liveClaims func(call time.Time) string, timeForm bool) (*models.Mock, string) {
	t.Helper()
	return learnNotifyIn(t, notifyBody, rec, liveClaims, timeForm)
}

// learnNotifyIn is learnNotify of a call whose body body writes of its
// token.
func learnNotifyIn(t *testing.T, body func(token string) string, rec string, liveClaims func(call time.Time) string, timeForm bool) (*models.Mock, string) {
	t.Helper()
	recToken := claimJWT(hs256Head, rec, recSig)
	m := learnRecorded("mock-0", 1300*time.Millisecond, "POST", "/v1/notify", body(recToken), notifyHeader(recToken), `{"ok":true}`)
	h, n, opts := learnOn(t)
	began := learnAt.Add(10 * time.Minute)
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.RecordedBegin(learnAt.Add(time.Second))
	tr.BeginAt(began)
	ctx := learn.WithTracker(context.Background(), tr)
	call := began.Add(300 * time.Millisecond)
	token := claimJWT(hs256Head, liveClaims(call), liveSig)
	in := live("POST", "/v1/notify", body(token), notifyHeader(token))
	in.arrived = call.Add(2 * time.Millisecond)
	ldb := &learnDb{perTest: []*models.Mock{m}}
	var db integrations.MockMemDb = ldb
	if timeForm {
		db = timedLearnDb{ldb}
	}
	served, miss, err := h.learnMatch(ctx, in, db, n, opts)
	if err != nil {
		t.Fatalf("learnMatch: %v", err)
	}
	if miss != nil {
		return nil, miss.res.Why
	}
	if served == nil || len(ldb.consumed) != 1 {
		t.Fatalf("served %v, consumed %d; want the recording served", served != nil, len(ldb.consumed))
	}
	c := ldb.consumed[0]
	return &c, ""
}

// The places a notify call's token times are learned at: each claim of the
// token in its body and in its header.
var notifyClaimKeys = []string{
	learn.ClaimClockKey("body.token", "exp"), learn.ClaimClockKey("body.token", "iat"),
	learn.ClaimClockKey("header.x-api-token", "exp"), learn.ClaimClockKey("header.x-api-token", "iat"),
}

// A JWT the application signs at each call, carried in a call's body and
// header, whose time claims (iat, exp) differ between the two record runs,
// is learned under the time default as those times, each checked by its
// form alone at its claim (models.ClockOffset.Claim), and nothing of the
// token as noise. Under strict times it stays refused, as before. A token
// one of whose other claims changed too, and one whose changed claim is no
// time (a moment learning did not read as the application's clock, under a
// name no JWT specification types a time), are not learned so.
func TestAJWTsTimeClaimsAreLearnedAsTimesCheckedByForm(t *testing.T) {
	recCall := learnAt.Add(1300 * time.Millisecond)
	rec := notifyClaims("svc-1", recCall, "")
	sameSub := func(call time.Time) string { return notifyClaims("svc-1", call, "") }

	t.Run("time default: the time claims learned by form", func(t *testing.T) {
		m, why := learnNotify(t, rec, sameSub, true)
		if m == nil {
			t.Fatalf("refused: %s; want the token's times learned by form", why)
		}
		for _, k := range notifyClaimKeys {
			o, ok := m.Spec.Clocks[k]
			claim := k[strings.LastIndex(k, ".")+1:]
			if !ok || o.Claim != claim || o.Form == "" {
				t.Fatalf("clocks %+v; want %s learned as a time in the %s claim, checked by form", m.Spec.Clocks, k, claim)
			}
		}
		if len(m.Spec.Clocks) != len(notifyClaimKeys) {
			t.Fatalf("clocks %+v; want only %v", m.Spec.Clocks, notifyClaimKeys)
		}
		for p := range m.Spec.ReqBodyNoise {
			if p == "body.token" || p == "header.x-api-token" {
				t.Fatalf("noise %v; want no noise at the token", m.Spec.ReqBodyNoise)
			}
		}
	})
	t.Run("time default: no claim key of a field whose name holds a '|'", func(t *testing.T) {
		barred := func(token string) string { return `{"to":"u-1","a|b":"` + token + `"}` }
		m, _ := learnNotifyIn(t, barred, rec, sameSub, true)
		if m == nil {
			return
		}
		for k := range m.Spec.Clocks {
			if strings.HasPrefix(k, "body.a|b|") {
				t.Fatalf("clocks %+v; want no key a replay cannot read as a claim's (%s)", m.Spec.Clocks, k)
			}
		}
	})
	t.Run("strict: refused as before", func(t *testing.T) {
		m, why := learnNotify(t, rec, sameSub, false)
		if m != nil {
			t.Fatalf("served with clocks %+v noise %v; want the token refused under strict times", m.Spec.Clocks, m.Spec.ReqBodyNoise)
		}
		if !strings.Contains(why, "no clock read by the app matched it") {
			t.Fatalf("refused: %s; want the token refused as no read of the clock", why)
		}
	})
	t.Run("time default: a non-time claim changed too", func(t *testing.T) {
		m, _ := learnNotify(t, rec, func(call time.Time) string { return notifyClaims("svc-2", call, "") }, true)
		if m != nil {
			for k := range m.Spec.Clocks {
				if strings.Contains(k, "|") {
					t.Fatalf("clocks %+v; want no claim learned where the token's sub changed", m.Spec.Clocks)
				}
			}
		}
	})
	t.Run("time default: a claim of a moment no read of the clock", func(t *testing.T) {
		// "ref" names a moment two days before each run's call: an instant
		// near the call, under a name no JWT specification types a time,
		// that learning read as no read of the clock.
		ref := func(at time.Time) string { return `,"ref":` + strconv.FormatInt(at.Add(-48*time.Hour).Unix(), 10) }
		m, _ := learnNotify(t, notifyClaims("svc-1", recCall, ref(recCall)), func(call time.Time) string { return notifyClaims("svc-1", call, ref(call)) }, true)
		if m != nil {
			for k := range m.Spec.Clocks {
				if strings.Contains(k, "|") {
					t.Fatalf("clocks %+v; want no claim learned where a claim that is no time changed", m.Spec.Clocks)
				}
			}
		}
	})
	t.Run("time default: a claim of another name read as the clock", func(t *testing.T) {
		issued := func(at time.Time) string { return `,"issued":` + strconv.FormatInt(at.Unix(), 10) }
		m, why := learnNotify(t, notifyClaims("svc-1", recCall, issued(recCall)), func(call time.Time) string { return notifyClaims("svc-1", call, issued(call)) }, true)
		if m == nil {
			t.Fatalf("refused: %s; want the token's times learned by form", why)
		}
		if o, ok := m.Spec.Clocks[learn.ClaimClockKey("body.token", "issued")]; !ok || o.Claim != "issued" {
			t.Fatalf("clocks %+v; want the issued claim, a read of the clock in its call's window, learned by form", m.Spec.Clocks)
		}
	})
}

// A replay of a call whose token times were learned by form at their claims
// passes a token whose times moved again, under the time default alone; a
// token whose other claim, header or signature form differs, or whose time
// is of another type, is refused, at the token's field, and nothing at a
// claim's key is checked as a leaf of its own.
func TestAJWTsTimeClaimsLearnedByFormAreCheckedAtReplay(t *testing.T) {
	recCall := learnAt.Add(1300 * time.Millisecond)
	m, why := learnNotify(t, notifyClaims("svc-1", recCall, ""), func(call time.Time) string { return notifyClaims("svc-1", call, "") }, true)
	if m == nil {
		t.Fatalf("refused at learn: %s", why)
	}
	base := learnAt.Add(2 * time.Hour)
	now := base.Add(time.Second)
	moved := notifyClaims("svc-1", now, "")
	both := []string{"body.token", "header.x-api-token"}
	never := []string{"^never$"}
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	for _, c := range []struct {
		name       string
		token      string
		timeForm   bool
		differs    []string
		why        string // each differing field's reason holds it
		headerPats map[string][]string
		bodyPats   map[string][]string
		sources    []models.DeclaredSource
	}{
		{name: "times moved, the time default", token: claimJWT(hs256Head, moved, liveSig), timeForm: true},
		{name: "the recorded token, the time default", token: claimJWT(hs256Head, notifyClaims("svc-1", recCall, ""), recSig), timeForm: true},
		{name: "the recorded token, strict times", token: claimJWT(hs256Head, notifyClaims("svc-1", recCall, ""), recSig)},
		{name: "times moved, another signature of the form", token: claimJWT(hs256Head, moved, strings.Repeat("A", len(recSig))), timeForm: true},
		{name: "times moved, strict times", token: claimJWT(hs256Head, moved, liveSig), differs: both, why: "learned under the time default"},
		{name: "another subject", token: claimJWT(hs256Head, notifyClaims("svc-2", now, ""), liveSig), timeForm: true, differs: both, why: "outside the times learned in its claims"},
		{name: "another header", token: claimJWT(`{"alg":"HS384","typ":"JWT"}`, moved, liveSig), timeForm: true, differs: both, why: "its header or another claim changed"},
		{name: "a signature of another length", token: claimJWT(hs256Head, moved, liveSig+"AAAA"), timeForm: true, differs: both, why: "signature is not of the recorded one's form"},
		{name: "a signature that is no base64url", token: claimJWT(hs256Head, moved, "+"+liveSig[1:]), timeForm: true, differs: both, why: "signature is not of the recorded one's form"},
		{name: "exp written as a string", token: claimJWT(hs256Head, `{"sub":"svc-1","iat":`+strconv.FormatInt(now.Unix(), 10)+`,"exp":"`+strconv.FormatInt(now.Unix()+3600, 10)+`"}`, liveSig), timeForm: true, differs: both, why: "exp claim was learned as a time"},
		{name: "a claim added", token: claimJWT(hs256Head, notifyClaims("svc-1", now, `,"role":"admin"`), liveSig), timeForm: true, differs: both, why: "another claim changed"},
		{name: "times moved, a header pattern the token is outside", token: claimJWT(hs256Head, moved, liveSig), timeForm: true, headerPats: map[string][]string{"x-api-token": never}, differs: []string{"header.x-api-token"}},
		{name: "times moved, a body pattern the token is outside", token: claimJWT(hs256Head, moved, liveSig), timeForm: true, bodyPats: map[string][]string{"token": never}, differs: []string{"body.token"}},
		{name: "times moved, a clock rule at the body token", token: claimJWT(hs256Head, moved, liveSig), timeForm: true,
			sources: []models.DeclaredSource{{Rule: "token-clock", Kind: models.DeclaredSourceClock, Field: "body.token", Clock: plain}}, differs: []string{"body.token"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", "1")
			rm := *m
			in := live("POST", "/v1/notify", notifyBody(c.token), notifyHeader(c.token))
			in.arrived = base.Add(2 * time.Second)
			db := sourcedDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{&rm}, updateUnFilteredReturn: true}, timeForm: c.timeForm,
				sources: map[string][]models.DeclaredSource{rm.Name: c.sources}}
			hn := models.ProvenFlakyHeaderNoise()
			for k, v := range c.headerPats {
				hn[k] = v
			}
			ok, stub, diag, err := newHTTP().match(clockConn(true, base), in, db, hn, c.bodyPats, nil, true, false, false, true, true)
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			if len(c.differs) == 0 {
				if !ok || stub == nil {
					t.Fatalf("refused (%v); want it answered", differs(diag))
				}
				return
			}
			if ok {
				t.Fatalf("answered; want refused naming %v", c.differs)
			}
			got := differs(diag)
			for _, f := range c.differs {
				if !hasField(got, f) {
					t.Fatalf("differs %v; want it to name %s", got, f)
				}
			}
			if len(got) != len(c.differs) {
				t.Fatalf("differs %v; want only %v", got, c.differs)
			}
			for _, f := range diag.differs {
				if c.why != "" && !strings.Contains(f.why, c.why) {
					t.Fatalf("%s refused for %q; want a reason that says %q", f.shape, f.why, c.why)
				}
			}
		})
	}
}

// The times learned in a token's claims are the ones at that field's own
// claim keys (learn.ClaimClockField): a claim's offset at a key of no claim,
// of another claim, or of a field whose name holds a '|', is none of them.
func TestATokensTimesAreTheOnesAtItsOwnClaimKeys(t *testing.T) {
	o := models.ClockOffset{Form: "number 9999999999", Claim: "exp"}
	k := strictKnown{clocks: map[string]models.ClockOffset{
		learn.ClaimClockKey("body.token", "exp"): o,
		"body.token":                             o,
		learn.ClaimClockKey("body.token", "iat"): o,
		learn.ClaimClockKey("body.a|b", "exp"):   o,
	}}
	if got := k.claimsAt("body.token"); len(got) != 1 || got[0] != o {
		t.Fatalf("times at body.token %+v; want the one at its exp claim's key", got)
	}
	if got := k.claimsAt("body.a|b"); len(got) != 0 {
		t.Fatalf("times at body.a|b %+v; want none", got)
	}
}
