package proxy

import (
	"fmt"
	"net/url"
	"strconv"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestWireFormURLEncodedRoundTripThroughCaptureLearnAndReplay feeds raw HTTP
// wire bytes containing encoded '&' (%26), '=' (%3D), '+' (%2B), '%' (%25),
// spaces, and an encoded redirect_uri with query parameters through
// IngressProxyManager.takeRequest (readTakenBody), into learn.Tracker.Incoming,
// through learn.Decide, and finally back out through httpint.RequestPlace under
// both exact Content-Type "application/x-www-form-urlencoded" and parameterized
// "application/x-www-form-urlencoded; charset=utf-8". Capture keeps the raw wire
// bytes so per-pair decoding preserves embedded delimiters without truncation
// or phantom keys, and invalid percent escapes fail closed.
func TestWireFormURLEncodedRoundTripThroughCaptureLearnAndReplay(t *testing.T) {
	rNanos := int64(1794207702156240454)
	fNanos := rNanos + int64(69*time.Second+3*time.Millisecond)
	rReqAt := time.Unix(0, rNanos).Add(131 * time.Microsecond)
	fReqAt := time.Unix(0, fNanos).Add(131 * time.Microsecond)
	rCallAt, fCallAt := rReqAt.Add(2*time.Millisecond), fReqAt.Add(2*time.Millisecond)
	ns := func(v int64) string { return strconv.FormatInt(v, 10) }

	rInst, fInst := "cli-inst-"+ns(rNanos), "cli-inst-"+ns(fNanos)
	rChal, fChal := "pkceRec+token/99", "pkceLive+token/88"
	rNote, fNote := "x&y=rec 50% off", "x&y=live 75% off"
	rRedirect := "https://app.example/cb?state=abc&nonce=" + ns(rNanos)
	fRedirect := "https://app.example/cb?state=abc&nonce=" + ns(fNanos)

	capture := func(ct, rawWireBody string, at time.Time) *models.HTTPReq {
		t.Helper()
		log := &requestLog{}
		log.wants.Store(true)
		pm := &IngressProxyManager{logger: zap.NewNop()}
		pm.SetRequestTaker(log)
		rawHTTP := fmt.Sprintf("POST /oauth/par HTTP/1.1\r\nHost: localhost:8083\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s",
			ct, len(rawWireBody), rawWireBody)
		pm.takeRequest(readRequest(t, rawHTTP), at)
		got := log.list()
		if len(got) != 1 || got[0].unread != "" {
			t.Fatalf("takeRequest(%s) = %+v; want 1 readable request", ct, got)
		}
		got[0].req.Timestamp = at
		return got[0].req
	}

	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"application/x-www-form-urlencoded; charset=utf-8",
	} {
		t.Run(ct, func(t *testing.T) {
			recWire := url.Values{
				"client_id":       {"demo-cli"},
				"redirect_uri":    {rRedirect},
				"installation_id": {rInst},
				"code_challenge":  {rChal},
				"note":            {rNote},
			}.Encode()
			liveWire := url.Values{
				"client_id":       {"demo-cli"},
				"redirect_uri":    {fRedirect},
				"installation_id": {fInst},
				"code_challenge":  {fChal},
				"note":            {fNote},
			}.Encode()

			recReq := capture(ct, recWire, rReqAt)
			liveReq := capture(ct, liveWire, fReqAt)
			wantRawFlag := ct == "application/x-www-form-urlencoded"
			if recReq.RawFormBody != wantRawFlag || liveReq.RawFormBody != wantRawFlag {
				t.Fatalf("RawFormBody = (rec=%v, live=%v); want %v for %q", recReq.RawFormBody, liveReq.RawFormBody, wantRawFlag, ct)
			}

			tr := learn.NewTracker()
			tr.SetClientTests(true)
			tr.Begin()
			tr.SetCases([]learn.ClientCase{{Name: "post-oauth-par-2", Req: recReq}})
			if !tr.Incoming(liveReq, "", fReqAt) {
				t.Fatal("Tracker.Incoming did not pair form request")
			}

			dc := tr.Drifts()
			placeAll := func(l learn.Leaf, _ bool) (string, bool) { return learn.VarPlace(l, true) }

			// 1. Learn + replay-read UnixNano word inside installation_id.
			ds := learn.Decide(learn.Input{
				Drifts:  dc,
				Drift:   []string{"body.docs.msgs.0.installation_id"},
				Rec:     learn.Shape{Leaves: map[string]learn.Leaf{"body.docs.msgs.0.installation_id": {Path: "body.docs.msgs.0.installation_id", Type: learn.String, Value: rInst}}},
				Live:    learn.Shape{Leaves: map[string]learn.Leaf{"body.docs.msgs.0.installation_id": {Path: "body.docs.msgs.0.installation_id", Type: learn.String, Value: fInst}}},
				At:      rCallAt,
				Arrived: fCallAt,
				Earlier: tr.Placed().Earlier,
				Caps:    learn.Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
				PlaceOf: placeAll,
			})
			if len(ds) != 1 || ds[0].Outcome != learn.OutcomeValue || !ds[0].From.Client || ds[0].FromAt.String() != "body.installation_id#2" {
				t.Fatalf("installation_id Decide = %+v; want OutcomeValue at body.installation_id#2", ds)
			}
			if val, why := httpint.RequestPlace(recReq, liveReq, ds[0].FromAt.String(), ds[0].Recorded); why != "" || val != ns(fNanos) {
				t.Fatalf("RequestPlace(%s) = (%q, %q); want (%q, \"\")", ds[0].FromAt.String(), val, why, ns(fNanos))
			}

			// 2. Replay-read whole fields with literal '+', '%', '&', and '=' without truncation or phantom keys.
			if val, why := httpint.RequestPlace(recReq, liveReq, "body.code_challenge", rChal); why != "" || val != fChal {
				t.Fatalf("RequestPlace(body.code_challenge) = (%q, %q); want (%q, \"\")", val, why, fChal)
			}
			if val, why := httpint.RequestPlace(recReq, liveReq, "body.note", rNote); why != "" || val != fNote {
				t.Fatalf("RequestPlace(body.note) = (%q, %q); want (%q, \"\")", val, why, fNote)
			}
			if val, why := httpint.RequestPlace(recReq, liveReq, "body.y", "rec 50% off"); why == "" {
				t.Fatalf("RequestPlace(body.y) = (%q, \"\"); phantom key from encoded &/= must be refused", val)
			}
			if val, why := httpint.RequestPlace(recReq, liveReq, "body.redirect_uri", rRedirect); why != "" || val != fRedirect {
				t.Fatalf("RequestPlace(body.redirect_uri) = (%q, %q); want (%q, \"\")", val, why, fRedirect)
			}
			if val, why := httpint.RequestPlace(recReq, liveReq, "body.nonce", ns(rNanos)); why == "" {
				t.Fatalf("RequestPlace(body.nonce) = (%q, \"\"); phantom key from encoded & inside redirect_uri must be refused", val)
			}

			// 3. Invalid escape anywhere in the form body fails closed (never reads raw "a+b%20c")
			// and capture preserves the raw wire bytes without dropping the request.
			badRec := capture(ct, "discount=50%&name=a+b%20c", rReqAt)
			badLive := capture(ct, "discount=50%&name=c+d%20e", fReqAt)
			if badRec.Body != "discount=50%&name=a+b%20c" {
				t.Fatalf("capture altered raw wire body: got %q", badRec.Body)
			}
			if val, why := httpint.RequestPlace(badRec, badLive, "body.name", "a+b%20c"); why == "" {
				t.Fatalf("RequestPlace on invalid-escape form = (%q, \"\"); want failure", val)
			}
			if val, why := httpint.RequestPlace(badRec, badLive, "body.name", "a b c"); why == "" {
				t.Fatalf("RequestPlace on invalid-escape form = (%q, \"\"); want failure", val)
			}

			// 4. Backward compatibility: legacy pre-decoded recordings paired with
			// raw live requests fail closed cleanly (never silently bind wrong data).
			legacyDecodedRec := &models.HTTPReq{
				Method:    "POST",
				URL:       "http://localhost:8083/oauth/par",
				Timestamp: rReqAt,
				Header:    map[string]string{"Content-Type": ct, "Host": "localhost:8083"},
				Body:      "redirect_uri=https://app.example/cb?state=abc&nonce=111&note=x&y=rec 50% off",
			}
			if val, why := httpint.RequestPlace(legacyDecodedRec, liveReq, "body.redirect_uri", rRedirect); why == "" {
				t.Fatalf("RequestPlace on legacy decoded recording = (%q, \"\"); want fail-closed mismatch", val)
			}

			// 5. Pairing: exact-CT legacy decoded recordings still pair cleanly
			// with out-of-order raw live requests captured through takeRequest,
			// while charset-CT raw recordings (which older builds already stored
			// raw) pair without cross-reading encoded delimiters.
			if ct == "application/x-www-form-urlencoded" {
				legacyCases := []learn.ClientCase{
					{Name: "post-signup-1", Req: &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/oauth/par", Header: map[string]string{"Content-Type": ct}, Body: "email=ann@example.com&plan=pro"}},
					{Name: "post-signup-2", Req: &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/oauth/par", Header: map[string]string{"Content-Type": ct}, Body: "email=bob@example.com&plan=pro"}},
				}
				liveBob := capture(ct, "email=bob%40example.com&plan=pro", fReqAt)
				liveAnn := capture(ct, "email=ann%40example.com&plan=pro", fReqAt.Add(time.Millisecond))
				idxBob, okBob := learn.PairCase(legacyCases, make([]bool, len(legacyCases)), liveBob, true)
				if !okBob || legacyCases[idxBob].Name != "post-signup-2" {
					t.Fatalf("legacy pair bob = (%d,%v); want post-signup-2", idxBob, okBob)
				}
				idxAnn, okAnn := learn.PairCase(legacyCases, make([]bool, len(legacyCases)), liveAnn, true)
				if !okAnn || legacyCases[idxAnn].Name != "post-signup-1" {
					t.Fatalf("legacy pair ann = (%d,%v); want post-signup-1", idxAnn, okAnn)
				}
				// Newly captured exact-CT raw recordings carry RawFormBody=true
				// and pair cleanly without encoded-delimiter tie swaps.
				rawExactCases := []learn.ClientCase{
					{Name: "post-amp", Req: capture(ct, "a=1&2", rReqAt)},
					{Name: "post-enc", Req: capture(ct, "a=1%262", rReqAt.Add(time.Millisecond))},
				}
				idxEnc, okEnc := learn.PairCase(rawExactCases, make([]bool, len(rawExactCases)), capture(ct, "a=1%262", fReqAt), true)
				if !okEnc || rawExactCases[idxEnc].Name != "post-enc" {
					t.Fatalf("exact raw pair enc = (%d,%v); want post-enc", idxEnc, okEnc)
				}
				idxAmp, okAmp := learn.PairCase(rawExactCases, make([]bool, len(rawExactCases)), capture(ct, "a=1&2", fReqAt), true)
				if !okAmp || rawExactCases[idxAmp].Name != "post-amp" {
					t.Fatalf("exact raw pair amp = (%d,%v); want post-amp", idxAmp, okAmp)
				}
				// Mixed legacy + re-recorded set pairs both ways cleanly.
				mixedCases := []learn.ClientCase{
					{Name: "s1-new", Req: capture(ct, "state=s1&redirect_uri=https%3A%2F%2Fa.io%2Fcb", rReqAt)},
					{Name: "s2-old", Req: &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/oauth/par", Header: map[string]string{"Content-Type": ct}, Body: "state=s2&redirect_uri=https://a.io/cb"}},
				}
				idxS1, okS1 := learn.PairCase(mixedCases, make([]bool, len(mixedCases)), capture(ct, "state=s1&redirect_uri=https%3A%2F%2Fa.io%2Fcb", fReqAt), true)
				if !okS1 || mixedCases[idxS1].Name != "s1-new" {
					t.Fatalf("mixed pair s1 = (%d,%v); want s1-new", idxS1, okS1)
				}
				idxS2, okS2 := learn.PairCase(mixedCases, make([]bool, len(mixedCases)), capture(ct, "state=s2&redirect_uri=https%3A%2F%2Fa.io%2Fcb", fReqAt), true)
				if !okS2 || mixedCases[idxS2].Name != "s2-old" {
					t.Fatalf("mixed pair s2 = (%d,%v); want s2-old", idxS2, okS2)
				}
			} else {
				rawCases := []learn.ClientCase{
					{Name: "post-enc", Req: &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/oauth/par", Header: map[string]string{"Content-Type": ct}, Body: "a=1%262"}},
					{Name: "post-amp", Req: &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/oauth/par", Header: map[string]string{"Content-Type": ct}, Body: "a=1&2"}},
				}
				liveAmp := capture(ct, "a=1&2", fReqAt)
				liveEnc := capture(ct, "a=1%262", fReqAt.Add(time.Millisecond))
				idxAmp, okAmp := learn.PairCase(rawCases, make([]bool, len(rawCases)), liveAmp, true)
				if !okAmp || rawCases[idxAmp].Name != "post-amp" {
					t.Fatalf("charset raw pair amp = (%d,%v); want post-amp", idxAmp, okAmp)
				}
				idxEnc, okEnc := learn.PairCase(rawCases, make([]bool, len(rawCases)), liveEnc, true)
				if !okEnc || rawCases[idxEnc].Name != "post-enc" {
					t.Fatalf("charset raw pair enc = (%d,%v); want post-enc", idxEnc, okEnc)
				}
			}
		})
	}
}
