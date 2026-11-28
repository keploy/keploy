package learn

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestAsyncReorderOfUnixNanoEmailBindsInsteadOfPick verifies that when a
// causally-prior in-scope fine-cut source already establishes a sub-millisecond
// value in both runs before a call, an async-reordered late answer (recorded
// after the call in R, served before it in F) does not trigger a RefusalPick
// or two-sources refusal. Genuine causality hazards (no earlier source, same
// mock answered late even beside another later send/answer, coarse cuts,
// out-of-scope earlier places, conflicting client slots, static/non-moving
// givens or slots even beside an earlier send, other-form places, swappable
// start-up answers, or a late answer that only holds round F's value or is
// unchanged beside an answer-only source) must stay refused.
func TestAsyncReorderOfUnixNanoEmailBindsInsteadOfPick(t *testing.T) {
	rNanos := int64(1764192000123456789)
	gap := 69*time.Second + 3*time.Millisecond
	fNanos := rNanos + gap.Nanoseconds()
	rBase := time.Unix(0, rNanos).Add(50 * time.Millisecond)
	fBase := time.Unix(0, fNanos).Add(50 * time.Millisecond)
	rCall, fCall := rBase.Add(50*time.Millisecond), fBase.Add(50*time.Millisecond)

	ns := func(v int64) string { return strconv.FormatInt(v, 10) }
	email := func(v int64) string { return "user-" + ns(v) + "@example.com" }
	rEmail, fEmail := email(rNanos), email(fNanos)
	rWord, fWord := ns(rNanos), ns(fNanos)

	emailMock := func(name, reqEmail, respEmail string, asked, answered time.Time) *models.Mock {
		return &models.Mock{
			Name: name,
			Kind: models.HTTP,
			Spec: models.MockSpec{
				ReqTimestampMock: asked,
				ResTimestampMock: answered,
				HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://db.local/query", Body: `{"email":"` + reqEmail + `"}`},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"user":{"email":"` + respEmail + `"}}`},
			},
		}
	}

	// mock-323: recorded AFTER mock-288 in R, served BEFORE mock-288 in F.
	lateRaw := func(recE, liveE string) Raw {
		asked, answered := rCall.Add(20*time.Millisecond), rCall.Add(21*time.Millisecond)
		return RawOf(emailMock("mock-323", recE, recE, asked, answered), true).
			ServedAs(1).
			ServedLive(RawOf(emailMock("mock-323", liveE, liveE, asked, answered), true))
	}
	lateEarlier := func(recE, liveE string) Earlier {
		asked, answered := rCall.Add(20*time.Millisecond), rCall.Add(21*time.Millisecond)
		return Earlier{
			Mock: "mock-323", At: asked, Own: true,
			Payload: RawOf(emailMock("mock-323", recE, recE, asked, answered), true).
				ServedAs(1).
				ServedLive(RawOf(emailMock("mock-323", liveE, liveE, asked, answered), true)),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.user.email", Noise: "body.user.email", Type: String, Value: recE}, Live: liveE}},
		}
	}

	const field = "body.docs.msgs.0.email"
	callWith := func(earlier []Earlier, raw []Raw) Input {
		in := iCall(field, String, rEmail, fEmail, rCall, fCall, iWin(rCall, fCall, 10*time.Millisecond), earlier, raw)
		in.Drifts = oClientDrifts()
		in.PlaceOf = func(l Leaf, _ bool) (string, bool) { return VarPlace(l, true) }
		return in
	}

	emailJWT := func(e string) string {
		enc := base64.RawURLEncoding
		h := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
		p := enc.EncodeToString([]byte(`{"email":"` + e + `","sid":"s-1"}`))
		return "keploy_session=" + h + "." + p + ".c2lnbmF0dXJl"
	}
	clientDecision := func(e string, at time.Time) *models.HTTPReq {
		return &models.HTTPReq{
			Method:    "POST",
			URL:       "http://localhost:8083/oauth/requests/req-1/decision",
			Timestamp: at,
			Header:    map[string]string{"Content-Type": "application/json", "Cookie": emailJWT(e), "Host": "localhost:8083"},
			Body:      `{"approve":true}`,
		}
	}

	checkExcusedLine := func(t *testing.T, what string, d Decision, wantMock, wantTarget string) {
		t.Helper()
		if !strings.Contains(d.Line, "; set aside ") ||
			!strings.Contains(d.Line, wantMock) ||
			!strings.Contains(d.Line, "which round F gave before the call but which was recorded after it") ||
			!strings.Contains(d.Line, wantTarget+" held the value before the call in both runs") {
			t.Errorf("%s: Line %q must name excused late answer %q and target %q", what, d.Line, wantMock, wantTarget)
		}
		wantPlace := Place{Mock: wantMock, Side: Response}
		if !slices.Contains(d.AnsweredAfter, wantPlace) {
			t.Errorf("%s: AnsweredAfter = %+v; want %+v", what, d.AnsweredAfter, wantPlace)
		}
	}

	t.Run("client request JWT cookie before call beats async reordered mock-323 (Raw and Earlier)", func(t *testing.T) {
		client := clientPlaced(t, "post-decision-4", clientDecision(rEmail, rBase), clientDecision(fEmail, fBase), fBase)
		d := oWord(t, Decide(callWith(client, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		if d.Outcome != OutcomeValue || !d.From.Client || d.From.Mock != "test case post-decision-4" {
			t.Fatalf("Raw late mock-323: %s; want OutcomeValue bound at client request", oDescribe([]Decision{d}))
		}
		checkExcusedLine(t, "Raw late mock-323", d, "mock-323", "test case post-decision-4 header.Cookie")
		d = oWord(t, Decide(callWith(append(client, lateEarlier(rEmail, fEmail)), nil)), rWord, fWord)
		if d.Outcome != OutcomeValue || !d.From.Client || d.From.Mock != "test case post-decision-4" {
			t.Fatalf("Earlier late mock-323: %s; want OutcomeValue bound at client request", oDescribe([]Decision{d}))
		}
		checkExcusedLine(t, "Earlier late mock-323", d, "mock-323", "test case post-decision-4 header.Cookie")
	})

	t.Run("placed Earlier late answer records exact Path in AnsweredAfter", func(t *testing.T) {
		client := clientPlaced(t, "post-decision-4", clientDecision(rEmail, rBase), clientDecision(fEmail, fBase), fBase)
		asked, answered := rBase.Add(10*time.Millisecond), rCall.Add(20*time.Millisecond)
		// Recorded response holds an unrelated value while round F served fEmail
		// early: answeredAfter does not record it, so excuseLate's aside Place
		// is the sole writer of {mock-323, Response, body.user.email}.
		placedLate := Earlier{
			Mock: "mock-323", At: asked, Own: true,
			Payload: RawOf(emailMock("mock-323", "other@example.com", "other@example.com", asked, answered), true).
				ServedAs(1).
				ServedLive(RawOf(emailMock("mock-323", fEmail, fEmail, asked, answered), true)),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.user.email", Noise: "body.user.email", Type: String, Value: "other@example.com"}, Live: fEmail}},
		}
		d := oWord(t, Decide(callWith(append(client, placedLate), nil)), rWord, fWord)
		if d.Outcome != OutcomeValue || !d.From.Client || d.From.Mock != "test case post-decision-4" {
			t.Fatalf("placed late mock-323: %s; want OutcomeValue bound at client request", oDescribe([]Decision{d}))
		}
		wantPlace := Place{Mock: "mock-323", Side: Response, Path: "body.user.email"}
		if !slices.Contains(d.AnsweredAfter, wantPlace) {
			t.Fatalf("AnsweredAfter = %+v; want %+v", d.AnsweredAfter, wantPlace)
		}
	})

	t.Run("distinct earlier in-scope send beats async reordered mock-323", func(t *testing.T) {
		earlierSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)
		d := oWord(t, Decide(callWith([]Earlier{earlierSend}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		iValue(t, "distinct earlier send beside late mock-323", d, "mock-260")
		checkExcusedLine(t, "distinct earlier send", d, "mock-323", "mock-260 body.email")
	})

	t.Run("earlier in-scope answer plus distinct earlier send beats async reordered mock-323", func(t *testing.T) {
		earlierAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		laterSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)
		d := oWord(t, Decide(callWith([]Earlier{earlierAns, laterSend}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		iValue(t, "earlier answer (5ms) + later send (10ms) beside late mock-323", d, "mock-250")
		checkExcusedLine(t, "earlier answer + later send", d, "mock-323", "mock-250 body.email")

		firstSend := iSent("mock-240", rBase.Add(2*time.Millisecond), "body.email", String, rEmail, fEmail)
		d = oWord(t, Decide(callWith([]Earlier{earlierAns, firstSend}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		iValue(t, "earlier send (2ms) + later answer (5ms) beside late mock-323", d, "mock-240")
		checkExcusedLine(t, "earlier send + later answer", d, "mock-323", "mock-240 body.email")
	})

	t.Run("earlier in-scope stated fine-cut answer beats async reordered duplicate mock-323", func(t *testing.T) {
		earlierAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		d := oWord(t, Decide(callWith([]Earlier{earlierAns}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		iValue(t, "stated fine-cut answer beside async duplicate mock-323", d, "mock-250")
		checkExcusedLine(t, "stated fine-cut answer", d, "mock-323", "mock-250 body.email")
	})

	t.Run("test-given fine-cut value beats async reordered mock-323", func(t *testing.T) {
		given := Earlier{
			Mock: "given:email", Given: true, Gave: true,
			Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "email", Noise: "email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		d := oWord(t, Decide(callWith([]Earlier{given}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		iValue(t, "given beside late mock-323", d, "given:email")
		checkExcusedLine(t, "given beside late mock-323", d, "mock-323", "given:email email")
	})

	// === NEGATIVE / CAUSALITY-PRESERVING GUARD TESTS ===

	t.Run("GUARD: no earlier source beside late mock-323 stays RefusalPick", func(t *testing.T) {
		d := oWord(t, Decide(callWith(nil, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "no earlier source", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: same mock asked before and answered after stays RefusalPick even beside later send or answer", func(t *testing.T) {
		same := RawOf(emailMock("mock-323", rEmail, rEmail, rBase, rCall.Add(20*time.Millisecond)), true).
			ServedAs(1).
			ServedLive(RawOf(emailMock("mock-323", fEmail, fEmail, rBase, rCall.Add(20*time.Millisecond)), true))
		d := oWord(t, Decide(callWith(nil, []Raw{same})), rWord, fWord)
		oRefused(t, "same mock answered late", d, RefusalPick, "mock-323", "recorded after it")

		// Adding a later stated answer (rBase+5ms) or later send (rBase+10ms)
		// must NOT excuse mock-323 because step 3/4 would still bind to
		// mock-323's own request half at rBase.
		laterAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		d = oWord(t, Decide(callWith([]Earlier{laterAns}, []Raw{same})), rWord, fWord)
		oRefused(t, "same mock answered late + later stated answer", d, RefusalPick, "mock-323", "recorded after it")

		laterSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)
		d = oWord(t, Decide(callWith([]Earlier{laterSend}, []Raw{same})), rWord, fWord)
		oRefused(t, "same mock answered late + later send", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: coarse second cut beside late mock-323 stays RefusalPick (send, client slot, and given)", func(t *testing.T) {
		// Use a second outside the call's 10ms window so readOverClients keeps
		// the client request in earlier and exercises excuseLate's fineCut guard.
		rSec, fSec := strconv.FormatInt(rBase.Add(-5*time.Second).Unix(), 10), strconv.FormatInt(fBase.Add(-5*time.Second).Unix(), 10)
		rCoarse, fCoarse := "e2e-sec-"+rSec+"@example.com", "e2e-sec-"+fSec+"@example.com"

		earlierSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rCoarse, fCoarse)
		in := iCall(field, String, rCoarse, fCoarse, rCall, fCall, iWin(rCall, fCall, 10*time.Millisecond), []Earlier{earlierSend}, []Raw{lateRaw(rCoarse, fCoarse)})
		in.Drifts = oClientDrifts()
		d := oWord(t, Decide(in), rSec, fSec)
		oRefused(t, "coarse second cut beside earlier send and late mock-323", d, RefusalPick, "mock-323", "recorded after it")

		clientCoarse := clientPlaced(t, "post-decision-4", clientDecision(rCoarse, rBase), clientDecision(fCoarse, fBase), fBase)
		inClient := iCall(field, String, rCoarse, fCoarse, rCall, fCall, iWin(rCall, fCall, 10*time.Millisecond), clientCoarse, []Raw{lateRaw(rCoarse, fCoarse)})
		inClient.Drifts = oClientDrifts()
		d = oWord(t, Decide(inClient), rSec, fSec)
		oRefused(t, "coarse second cut beside client request slot and late mock-323", d, RefusalPick, "mock-323", "recorded after it")

		givenCoarse := Earlier{
			Mock: "given:email", Given: true, Gave: true,
			Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "email", Noise: "email", Type: String, Value: rCoarse}, Live: fCoarse}},
		}
		inGiven := iCall(field, String, rCoarse, fCoarse, rCall, fCall, iWin(rCall, fCall, 10*time.Millisecond), []Earlier{givenCoarse}, []Raw{lateRaw(rCoarse, fCoarse)})
		inGiven.Drifts = oClientDrifts()
		d = oWord(t, Decide(inGiven), rSec, fSec)
		oRefused(t, "coarse second cut beside test-given and late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: static unchanged or non-moving or other-form test-given stays RefusalPick even beside an earlier send", func(t *testing.T) {
		earlierSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)

		both := rEmail + " " + fEmail
		unchangedGiven := Earlier{
			Mock: "given:email", Given: true, Gave: true,
			Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "email", Noise: "email", Type: String, Value: both}, Live: both}},
		}
		d := oWord(t, Decide(callWith([]Earlier{unchangedGiven}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "static unchanged given beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")

		// Even with an earlier send present, step 2 would bind to the unchanged given,
		// so the earlier send must NOT excuse late mock-323.
		d = oWord(t, Decide(callWith([]Earlier{unchangedGiven, earlierSend}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "static unchanged given + earlier send beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")

		// Given whose live text still holds R's moment (did not move cleanly).
		nonMovingGiven := Earlier{
			Mock: "given:email", Given: true, Gave: true,
			Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "email", Noise: "email", Type: String, Value: rEmail}, Live: rEmail + " " + fEmail}},
		}
		d = oWord(t, Decide(callWith([]Earlier{nonMovingGiven}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "non-moving given beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")

		// Given holding the moment in another form (asText false).
		rISO := time.Unix(0, rNanos).UTC().Format(time.RFC3339Nano)
		fISO := time.Unix(0, fNanos).UTC().Format(time.RFC3339Nano)
		otherFormGiven := Earlier{
			Mock: "given:email", Given: true, Gave: true,
			Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "email", Noise: "email", Type: String, Value: rISO}, Live: fISO}},
		}
		d = oWord(t, Decide(callWith([]Earlier{otherFormGiven}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "other-form given beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: out-of-scope earlier send beside late mock-323 stays RefusalPick", func(t *testing.T) {
		otherSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)
		otherSend.Own, otherSend.Run = false, true
		d := oWord(t, Decide(callWith([]Earlier{otherSend}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "out-of-scope earlier send", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: two conflicting client slots beside late mock-323 stays RefusalPick", func(t *testing.T) {
		c1 := clientDecision("a-"+rEmail, rBase)
		c2 := clientDecision("b-"+rEmail, rBase.Add(5*time.Millisecond))
		f1 := clientDecision("a-"+fEmail, fBase)
		f2 := clientDecision("b-"+fEmail, fBase.Add(5*time.Millisecond))
		tr := NewTracker()
		tr.Begin()
		tr.SetCases([]ClientCase{{Name: "post-a-1", Req: c1}, {Name: "post-b-2", Req: c2}})
		tr.Incoming(f1, "", f1.Timestamp)
		tr.Incoming(f2, "", f2.Timestamp)
		d := oWord(t, Decide(callWith(tr.Placed().Earlier, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "conflicting client slots beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: client slot that did not move stays RefusalPick even beside an earlier send", func(t *testing.T) {
		client := clientPlaced(t, "post-decision-4",
			clientDecision(rEmail, rBase),
			clientDecision(rEmail+" "+fEmail, fBase), fBase)
		d := oWord(t, Decide(callWith(client, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "non-moving client slot beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")

		earlierSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)
		d = oWord(t, Decide(callWith(append(client, earlierSend), []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "non-moving client slot + earlier send beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: client slot holding moment in another form (asText false) stays RefusalPick", func(t *testing.T) {
		rISO := time.Unix(0, rNanos).UTC().Format(time.RFC3339Nano)
		fISO := time.Unix(0, fNanos).UTC().Format(time.RFC3339Nano)
		client := clientPlaced(t, "post-decision-4",
			clientDecision(rISO, rBase),
			clientDecision(fISO, fBase), fBase)
		d := oWord(t, Decide(callWith(client, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "other-form client slot beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: answer-only source beside late mock-323 holding only F's value stays RefusalPick", func(t *testing.T) {
		earlierAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		// mock-323 held an unrelated email in R, and only holds F's email in F.
		d := oWord(t, Decide(callWith([]Earlier{earlierAns}, []Raw{lateRaw("other@example.com", fEmail)})), rWord, fWord)
		oRefused(t, "late mock-323 without R's value beside answer-only source", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: answer-only source beside unchanged late mock-323 holding both moments stays RefusalPick", func(t *testing.T) {
		earlierAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		both := rEmail + " " + fEmail
		d := oWord(t, Decide(callWith([]Earlier{earlierAns}, []Raw{lateRaw(both, both)})), rWord, fWord)
		oRefused(t, "unchanged late mock-323 beside answer-only source", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: answer-only source holding both moments (did not move) beside late mock-323 stays RefusalPick", func(t *testing.T) {
		earlierAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: rEmail + " " + fEmail}},
		}
		d := oWord(t, Decide(callWith([]Earlier{earlierAns}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "non-moving earlier answer beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: answer-only source holding moment in another form (byText false) beside late mock-323 stays RefusalPick", func(t *testing.T) {
		rISO := time.Unix(0, rNanos).UTC().Format(time.RFC3339Nano)
		fISO := time.Unix(0, fNanos).UTC().Format(time.RFC3339Nano)
		earlierAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.at", Noise: "body.at", Type: String, Value: rISO}, Live: fISO}},
		}
		d := oWord(t, Decide(callWith([]Earlier{earlierAns}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "other-form earlier answer beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: answer-only source with unstated F view beside late mock-323 stays RefusalPick", func(t *testing.T) {
		unstatedAns := RawOf(emailMock("mock-250", "other@example.com", rEmail, rBase, rBase.Add(5*time.Millisecond)), true).ServedAs(1)
		d := oWord(t, Decide(callWith(nil, []Raw{unstatedAns, lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "unstated earlier answer beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")

		// Even when ServedLive is populated, Caps.FViewStated=false marks the
		// earlier answer fUnstated (before(q) is false) so it must stay refused.
		statedRaw := RawOf(emailMock("mock-250", "other@example.com", rEmail, rBase, rBase.Add(5*time.Millisecond)), true).
			ServedAs(1).
			ServedLive(RawOf(emailMock("mock-250", "other@example.com", fEmail, rBase, rBase.Add(5*time.Millisecond)), true))
		in := callWith(nil, []Raw{statedRaw, lateRaw(rEmail, fEmail)})
		in.Caps.FViewStated = false
		d = oWord(t, Decide(in), rWord, fWord)
		oRefused(t, "earlier answer with FViewStated=false beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: unstated earliest answer a0 + later stated answer q + late mock-323 stays RefusalPick", func(t *testing.T) {
		// Step 3 binds at earliest(answers) (mock-240, unstated F view). A
		// later stated answer (mock-250) must NOT excuse late mock-323 because
		// replay would bind at mock-240, not mock-250.
		a0Unstated := RawOf(emailMock("mock-240", "other@example.com", rEmail, rBase.Add(1*time.Millisecond), rBase.Add(2*time.Millisecond)), true).ServedAs(1)
		qStated := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		d := oWord(t, Decide(callWith([]Earlier{qStated}, []Raw{a0Unstated, lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "unstated a0 + stated q + late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: step 3b validates chosenTarget (earliest answer), not last answer in slice", func(t *testing.T) {
		// Case 1: answers[0] (earliest, mock-240) is non-moving while
		// answers[1] (later, mock-250) is valid. chosenTarget picks mock-240
		// so late mock-323 must stay refused rather than being excused by
		// answers[len-1].
		a0NonMoving := Earlier{
			Mock: "mock-240", At: rBase.Add(1 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: rEmail + " " + fEmail}},
		}
		qValid := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		d := oWord(t, Decide(callWith([]Earlier{a0NonMoving, qValid}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "earliest non-moving a0 + later valid q + late mock-323", d, RefusalPick, "mock-323", "recorded after it")

		// Case 2: answers[0] (earliest, mock-240) is valid while answers[1]
		// (later, mock-250) is non-moving. chosenTarget picks mock-240 so late
		// mock-323 is excused and the decision binds at mock-240.
		a0Valid := Earlier{
			Mock: "mock-240", At: rBase.Add(1 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		qNonMoving := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: rEmail + " " + fEmail}},
		}
		d = oWord(t, Decide(callWith([]Earlier{a0Valid, qNonMoving}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		iValue(t, "earliest valid a0 + later non-moving q + late mock-323", d, "mock-240")
		checkExcusedLine(t, "earliest valid a0 + later non-moving q", d, "mock-323", "mock-240 body.email")
	})

	t.Run("GUARD: out-of-scope earlier answer beside late mock-323 stays RefusalPick", func(t *testing.T) {
		otherAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: false, Run: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		d := oWord(t, Decide(callWith([]Earlier{otherAns}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "out-of-scope earlier answer beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: earlier answer before later send does not bypass answer-binding guards", func(t *testing.T) {
		earlierAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		laterSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)
		// Step 3 would bind at mock-250 (earlier than mock-260), so late mock-323
		// holding only F's value must still be refused rather than excused by mock-260.
		d := oWord(t, Decide(callWith([]Earlier{earlierAns, laterSend}, []Raw{lateRaw("other@example.com", fEmail)})), rWord, fWord)
		oRefused(t, "earlier answer + later send beside late mock-323 holding only F", d, RefusalPick, "mock-323", "recorded after it")

		// Same when the earlier answer has FViewStated=false: Step 3 would still
		// bind at mock-250, so mock-260 (after mock-250) must not excuse late mock-323.
		unstatedAns := RawOf(emailMock("mock-250", "other@example.com", rEmail, rBase, rBase.Add(5*time.Millisecond)), true).
			ServedAs(1).
			ServedLive(RawOf(emailMock("mock-250", "other@example.com", fEmail, rBase, rBase.Add(5*time.Millisecond)), true))
		in := callWith([]Earlier{laterSend}, []Raw{unstatedAns, lateRaw(rEmail, fEmail)})
		in.Caps.FViewStated = false
		d = oWord(t, Decide(in), rWord, fWord)
		oRefused(t, "unstated earlier answer + later send beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: answer-only source beside late mock-323 holding coarse cut in R or F stays RefusalPick", func(t *testing.T) {
		// Exact-millisecond UnixNano moments (res=1ns, fine) share the same
		// moment key with RFC3339 millisecond strings (res=1ms, not fineCut),
		// so holds returns hr/hl=true with fr/fl=false.
		rMsNanos := int64(1764192000123000000)
		fMsNanos := int64(1764192069126000000)
		rMsEmail, fMsEmail := email(rMsNanos), email(fMsNanos)
		rMsWord, fMsWord := ns(rMsNanos), ns(fMsNanos)
		rMsISO := time.Unix(0, rMsNanos).UTC().Format("2006-01-02T15:04:05.000Z")
		fMsISO := time.Unix(0, fMsNanos).UTC().Format("2006-01-02T15:04:05.000Z")

		earlierAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rMsEmail}, Live: fMsEmail}},
		}
		callMs := func(raw []Raw) Input {
			in := iCall(field, String, rMsEmail, fMsEmail, rCall, fCall, iWin(rCall, fCall, 10*time.Millisecond), []Earlier{earlierAns}, raw)
			in.Drifts = oClientDrifts()
			in.PlaceOf = func(l Leaf, _ bool) (string, bool) { return VarPlace(l, true) }
			return in
		}

		// Coarse 1ms cut in R (late.hr=true, late.fr=false, late.hl=true, late.fl=true).
		d := oWord(t, Decide(callMs([]Raw{lateRaw(rMsISO, fMsEmail)})), rMsWord, fMsWord)
		oRefused(t, "late mock-323 coarse in R beside answer-only source", d, RefusalPick, "mock-323", "recorded after it")

		// Coarse 1ms cut in F (late.hr=true, late.fr=true, late.hl=true, late.fl=false).
		d = oWord(t, Decide(callMs([]Raw{lateRaw(rMsEmail, fMsISO)})), rMsWord, fMsWord)
		oRefused(t, "late mock-323 coarse in F beside answer-only source", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: typed microsecond instant with coarse earlier answer in R or F stays RefusalPick", func(t *testing.T) {
		rMsNanos := int64(1764192000123000000)
		fMsNanos := int64(1764192069126000000)
		rMicro := time.Unix(0, rMsNanos).UTC().Format("2006-01-02T15:04:05.000000Z")
		fMicro := time.Unix(0, fMsNanos).UTC().Format("2006-01-02T15:04:05.000000Z")
		rMsISO := time.Unix(0, rMsNanos).UTC().Format("2006-01-02T15:04:05.000Z")
		fMsISO := time.Unix(0, fMsNanos).UTC().Format("2006-01-02T15:04:05.000Z")

		// Earlier answer coarse (1ms) in R (h.hr=true, h.fr=false, h.hl=true, h.fl=true).
		coarseR := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.at", Noise: "body.at", Type: String, Value: rMsISO}, Live: fMicro}},
		}
		inR := iCall(field, Instant, rMicro, fMicro, rCall, fCall, iWin(rCall, fCall, 10*time.Millisecond), []Earlier{coarseR}, []Raw{lateRaw(rMicro, fMicro)})
		inR.Drifts = oClientDrifts()
		d := oWord(t, Decide(inR), rMicro, fMicro)
		oRefused(t, "typed instant with coarse-in-R earlier answer", d, RefusalPick, "mock-323", "recorded after it")

		// Earlier answer coarse (1ms) in F (h.hr=true, h.fr=true, h.hl=true, h.fl=false).
		coarseF := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.at", Noise: "body.at", Type: String, Value: rMicro}, Live: fMsISO}},
		}
		inF := iCall(field, Instant, rMicro, fMicro, rCall, fCall, iWin(rCall, fCall, 10*time.Millisecond), []Earlier{coarseF}, []Raw{lateRaw(rMicro, fMicro)})
		inF.Drifts = oClientDrifts()
		d = oWord(t, Decide(inF), rMicro, fMicro)
		oRefused(t, "typed instant with coarse-in-F earlier answer", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: test-given holding only F's value beside late mock-323 stays RefusalPick", func(t *testing.T) {
		given := Earlier{
			Mock: "given:email", Given: true, Gave: true,
			Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "email", Noise: "email", Type: String, Value: "other@example.com"}, Live: fEmail}},
		}
		d := oWord(t, Decide(callWith([]Earlier{given}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		oRefused(t, "given holding only F beside late mock-323", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("client slot beside finer-instant cutSlot client request beats async reordered mock-323", func(t *testing.T) {
		// Call holds a sub-ms microsecond timestamp outside the call's 10ms window
		// (so readOverClients keeps client requests) yet inside sinceBegin (so
		// windowRead is true and client-2 holding the finer UnixNano is a cutSlot).
		rUs := time.Unix(0, rNanos).UTC().Truncate(time.Microsecond)
		fUs := time.Unix(0, fNanos).UTC().Truncate(time.Microsecond)
		rUsWord := strconv.FormatInt(rUs.UnixMicro(), 10)
		fUsWord := strconv.FormatInt(fUs.UnixMicro(), 10)
		rUsEmail := "e2e-us-" + rUsWord + "@example.com"
		fUsEmail := "e2e-us-" + fUsWord + "@example.com"

		// client-1 holds exact microsecond text; client-2 holds strictly finer UnixNano.
		c1Rec := clientDecision(rUsEmail, rBase)
		c1Live := clientDecision(fUsEmail, fBase)
		c2Rec := clientDecision(rEmail, rBase.Add(5*time.Millisecond))
		c2Live := clientDecision(fEmail, fBase.Add(5*time.Millisecond))
		tr := NewTracker()
		tr.Begin()
		tr.SetCases([]ClientCase{{Name: "post-exact-1", Req: c1Rec}, {Name: "post-finer-2", Req: c2Rec}})
		tr.Incoming(c1Live, "", c1Live.Timestamp)
		tr.Incoming(c2Live, "", c2Live.Timestamp)

		win := iWin(rCall, fCall, 10*time.Millisecond)
		win.Rec.Began = time.Unix(0, rNanos).Add(-5 * time.Millisecond)
		win.F.Began = time.Unix(0, fNanos).Add(-5 * time.Millisecond)
		in := iCall(field, String, rUsEmail, fUsEmail, rCall, fCall, win, tr.Placed().Earlier, []Raw{lateRaw(rUsEmail, fUsEmail)})
		in.Drifts = oClientDrifts()
		in.PlaceOf = func(l Leaf, _ bool) (string, bool) { return VarPlace(l, true) }
		d := oWord(t, Decide(in), rUsWord, fUsWord)
		if d.Outcome != OutcomeValue || !d.From.Client || d.From.Mock != "test case post-exact-1" {
			t.Fatalf("exact slot beside finer cutSlot + late mock-323: %s; want OutcomeValue bound at post-exact-1", oDescribe([]Decision{d}))
		}
	})

	t.Run("GUARD: late swappable start-up answer beside earlier send stays refused with swapNote", func(t *testing.T) {
		earlierSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)
		asked, answered := rCall.Add(20*time.Millisecond), rCall.Add(21*time.Millisecond)
		swappableLate := RawOf(emailMock("mock-323", rEmail, rEmail, asked, answered), false).
			ServedAs(1).
			ServedLive(RawOf(emailMock("mock-323", fEmail, fEmail, asked, answered), false))
		swappableLate.startUp = true
		otherStartUp := RawOf(emailMock("mock-324", "other@example.com", "other@example.com", asked, answered), false)
		otherStartUp.startUp = true
		d := oWord(t, Decide(callWith([]Earlier{earlierSend}, []Raw{swappableLate, otherStartUp})), rWord, fWord)
		oRefused(t, "swappable late start-up answer beside earlier send", d, RefusalPick, "mock-323", "swap")
	})

	t.Run("GUARD: unbindable JWT earlier send falling back to late mock's own request half stays RefusalPick", func(t *testing.T) {
		// M1 holds the email only inside a signed JWT claim (unbindable) at rBase-1ms,
		// while mock-323 is asked at rBase (request half is bindable) and answered
		// after the call in R and before it in F. chosenTarget(fineCopies) skips M1
		// and selects mock-323's request half; excuseLate must see target.mock == late.p.mock
		// and refuse (both in Step 4 and in Step 3a beside a later stated answer).
		jwtSend := func(email string) string {
			hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
			pay := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"` + email + `"}`))
			return hdr + "." + pay + ".sig"
		}
		m1 := iSent("mock-100", rBase.Add(-1*time.Millisecond), "header.Authorization", String, "Bearer "+jwtSend(rEmail), "Bearer "+jwtSend(fEmail))
		same := RawOf(emailMock("mock-323", rEmail, rEmail, rBase, rCall.Add(20*time.Millisecond)), true).
			ServedAs(1).
			ServedLive(RawOf(emailMock("mock-323", fEmail, fEmail, fBase, fBase.Add(1*time.Millisecond)), true))
		d := oWord(t, Decide(callWith([]Earlier{m1}, []Raw{same})), rWord, fWord)
		oRefused(t, "JWT fallback to same-mock request half (step 4)", d, RefusalPick, "mock-323", "recorded after it")

		laterAns := Earlier{
			Mock: "mock-250", At: rBase.Add(5 * time.Millisecond), Own: true,
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "body.email", Noise: "body.email", Type: String, Value: rEmail}, Live: fEmail}},
		}
		d = oWord(t, Decide(callWith([]Earlier{m1, laterAns}, []Raw{same})), rWord, fWord)
		oRefused(t, "JWT fallback to same-mock request half (step 3a)", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("GUARD: late answer served before the test or in an older test stays RefusalPick", func(t *testing.T) {
		earlierSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)
		beforeTestLate := lateRaw(rEmail, fEmail).ServedBeforeTheTest()
		d := oWord(t, Decide(callWith([]Earlier{earlierSend}, []Raw{beforeTestLate})), rWord, fWord)
		oRefused(t, "late answer served before test", d, RefusalPick, "mock-323", "recorded after it")

		olderTestLate := lateRaw(rEmail, fEmail).ServedInAnOlderTest()
		d = oWord(t, Decide(callWith([]Earlier{earlierSend}, []Raw{olderTestLate})), rWord, fWord)
		oRefused(t, "late answer served in older test", d, RefusalPick, "mock-323", "recorded after it")
	})

	t.Run("multiple distinct late answers truncate at maxLinePlaces with ellipsis and populate AnsweredAfter", func(t *testing.T) {
		earlierSend := iSent("mock-260", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)
		var lates []Raw
		for i := 0; i < maxLinePlaces+2; i++ {
			name := fmt.Sprintf("mock-%d", 330+i)
			asked := rCall.Add(time.Duration(20+i*2) * time.Millisecond)
			answered := asked.Add(time.Millisecond)
			r := RawOf(emailMock(name, rEmail, rEmail, asked, answered), true).
				ServedAs(1).
				ServedLive(RawOf(emailMock(name, fEmail, fEmail, asked, answered), true))
			lates = append(lates, r)
		}
		d := oWord(t, Decide(callWith([]Earlier{earlierSend}, lates)), rWord, fWord)
		if d.Outcome != OutcomeValue || d.From.Mock != "mock-260" {
			t.Fatalf("expected OutcomeValue bound at mock-260; got %s", oDescribe([]Decision{d}))
		}
		if !strings.Contains(d.Line, ", …") {
			t.Fatalf("expected maxLinePlaces ellipsis in line; got %q", d.Line)
		}
		if strings.Count(d.Line, "held the value before the call in both runs") != 1 {
			t.Fatalf("expected reason once in truncated line; got %q", d.Line)
		}
		if len(d.AnsweredAfter) != maxLinePlaces+2 {
			t.Fatalf("AnsweredAfter = %+v; want %d entries", d.AnsweredAfter, maxLinePlaces+2)
		}
	})

	t.Run("GUARD: OutcomeRefused beside excused late answer leaves AnsweredAfter empty", func(t *testing.T) {
		// Two earlier sends where the second was its own clock read produce
		// RefusalTwoSources inside copied() after excuseLate passes; withAside
		// must omit AnsweredAfter when the decision is refused.
		sendA := iSent("mock-260", rBase.Add(5*time.Millisecond), "body.email", String, rEmail, fEmail)
		sendB := iSent("mock-261", rBase.Add(10*time.Millisecond), "body.email", String, rEmail, fEmail)
		sendB.Seen[0].Clock = true
		d := oWord(t, Decide(callWith([]Earlier{sendA, sendB}, []Raw{lateRaw(rEmail, fEmail)})), rWord, fWord)
		if d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalTwoSources {
			t.Fatalf("Outcome = %v refusal = %+v; want RefusalTwoSources; line=%q", d.Outcome, d.Refusal, d.Line)
		}
		if len(d.AnsweredAfter) != 0 {
			t.Fatalf("AnsweredAfter = %+v; want empty on OutcomeRefused", d.AnsweredAfter)
		}
	})
}
