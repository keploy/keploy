package learn

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A value that holds an '=' no quoted-printable escape starts is no
// quoted-printable text, in either run, whatever its other escapes show:
// the "=XX" its signature happens to begin with in one run is no escape.
// An AWS SigV4 Authorization header, as a .NET AWS SDK signed a DynamoDB
// UpdateItem in each run (the recorded signature begins "09", an "=09"
// that reads as a tab; the live one "b1", which reads as nothing), changed
// only in its signature: the change is the signature's, which no reading
// refuses, never "a run of the base64 alphabet that does not decode" that
// only a misread escape made.
func TestAnEqualsSignNoEscapeStartsIsNoQuotedPrintableInEitherRun(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 385340565, time.UTC)
	const head = "AWS4-HMAC-SHA256 Credential=local/20261102/us-east-1/dynamodb/aws4_request, SignedHeaders=content-type;host;x-amz-api-version;x-amz-content-sha256;x-amz-date;x-amz-target, Signature="
	for _, tc := range []struct{ name, rec, live string }{
		// As the PoC's replay saw it: the recorded run's "=09" a tab, the
		// live run's "=b1" no escape.
		{"aws signature, recorded escape-like", head + "09bbdba4c503009bffa4d5df085bfc642559eb27e81e28586a31d6fa5cf13e5b", head + "b17da81317308f9dc505ba7b017f223e47ef887b56beb75c5b8d1f34ac6820d9"},
		// The other way round: the live run's "=23" a '#'.
		{"aws signature, live escape-like", head + "f9ca371aebde9b4cde951034be2ecc4d7f0197e39dbc4700dad429a0ddbcbbd2", head + "23c82f485e7e7d80389cf2378fca46cf8050e868bcc4ede7222a0600816f67a9"},
		// One '=' alone, upper-case hex: "=09" a tab in one run, "=B1" no
		// text in the other.
		{"one token, recorded escape-like", "Signature=09ABCDEF0123456789", "Signature=B1CDEF0123456789AB"},
		// The other way round: each run's reading counts, not the
		// recorded one's only.
		{"one token, live escape-like", "Signature=B1CDEF0123456789AB", "Signature=09ABCDEF0123456789"},
		// An "=3D" in both runs shows quoted-printable, but the run whose
		// signature begins "=b1" holds an '=' that starts no escape: that
		// run is still no quoted-printable text, so neither is read in that
		// form.
		{"escaped '=' in both runs, recorded escape-like", "k=3Dv&Signature=09bbdba4c503009bffa4d5df085bfc64", "k=3Dv&Signature=b17da81317308f9dc505ba7b017f223e"},
		{"escaped '=' in both runs, live escape-like", "k=3Dv&Signature=b17da81317308f9dc505ba7b017f223e", "k=3Dv&Signature=09bbdba4c503009bffa4d5df085bfc64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOne(tc.rec, tc.live, at, nil, nil, nil); got.Kind != KindNoise {
				t.Fatalf("got %s (%s), want noise: its change is a token's, read raw", got.Kind, got.Why)
			}
		})
	}
}

// A value that reads as quoted-printable text in both runs, whole or as a
// mail that says it is (its header lines hold an '=' of their own,
// "charset=utf-8", that starts no escape), is still read in that form: an
// id a soft line break splits in one run is read whole there, and one an
// escape hides is found, so the id that flowed from an earlier call is
// followed as the variable it is.
func TestQuotedPrintableTextStillReadsItsForm(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	earlier := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.id", Noise: "body.id", Type: String, Value: "A7K29Q"}, Live: "B8L30R", Place: "body.id"},
	}}}
	mail := func(header, id string) string {
		return "From: shop@test\r\nSubject: Your order\r\nContent-Type: text/html; charset=utf-8\r\n" + header + "\r\n\r\n" +
			"<p>Thank you</p><a href=3D\"https://shop.test/track?order=3D" + id + "&lang=3Den\">Track</a>\r\n"
	}
	for _, tc := range []struct{ name, rec, live string }{
		{"soft break in one run", "order A7K=\r\n29Q placed", "order B8L30R placed"},
		{"escape in both runs", "order=3DA7K29Q", "order=3DB8L30R"},
		{"a mail that says it is", mail("Content-Transfer-Encoding: quoted-printable", "A7K29Q"), mail("Content-Transfer-Encoding: quoted-printable", "B8L30R")},
		{"a mail that says it is, in lower case", mail("content-transfer-encoding:quoted-printable", "A7K29Q"), mail("content-transfer-encoding:quoted-printable", "B8L30R")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOne(tc.rec, tc.live, at, earlier, nil, nil); got.Kind != KindVariable {
				t.Fatalf("got %s (%s), want the id that flowed followed as a variable", got.Kind, got.Why)
			}
		})
	}
}

// Two signatures in upper-case hex (as .NET's Convert.ToHexString, Java's
// printHexBinary and Go's %X write them), 64 digits each.
const (
	sigA = "F0A1C2E3D4B5A69788796A5B4C3D2E1F0F1E2D3C4B5A69788796A5B4C3D2E1F0"
	sigB = "7C4E19B2A0D35F8E61C7B9042A3E5D8F1B6C0E9A2D4F7B3C5E8A1D6F0B2C4E97"
)

// A signed value in upper-case hex whose first two digits happen to be an
// escape of a character quoted-printable may write as itself ("2B" a '+',
// "41" an 'A', "20" a space, "09" a tab) is no quoted-printable text: its
// change is the signature's, read raw, in either run, against a signature
// that begins with any two digits, "3D" (an '=', which quoted-printable has
// to escape) too.
func TestASignatureInUpperCaseHexIsNoQuotedPrintable(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 385340565, time.UTC)
	for _, tc := range []struct{ name, rec, live string }{
		{"'+' against a tab", "sha256=2B" + sigA[2:], "sha256=09" + sigB[2:]},
		{"a tab against '+'", "sha256=09" + sigA[2:], "sha256=2B" + sigB[2:]},
		{"'=' against 'A'", "v1=3D" + sigA[2:], "v1=41" + sigB[2:]},
		{"'A' against '='", "v1=41" + sigA[2:], "v1=3D" + sigB[2:]},
		{"'=' against a space", "v1=3D" + sigA[2:], "v1=20" + sigB[2:]},
		{"a space against '='", "v1=20" + sigA[2:], "v1=3D" + sigB[2:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOne(tc.rec, tc.live, at, nil, nil, nil); got.Kind != KindNoise {
				t.Fatalf("got %s (%s), want noise: its change is a signature's, read raw", got.Kind, got.Why)
			}
		})
	}
	t.Run("any first two digits against '='", func(t *testing.T) {
		const digits = "0123456789ABCDEF"
		for _, pre := range []string{"v1=", "sha256="} {
			for _, hi := range digits {
				for _, lo := range digits {
					xy := string(hi) + string(lo)
					for _, p := range [][2]string{{pre + xy + sigA[2:], pre + "3D" + sigB[2:]}, {pre + "3D" + sigA[2:], pre + xy + sigB[2:]}} {
						if got := classifyOne(p[0], p[1], at, nil, nil, nil); got.Kind != KindNoise {
							t.Errorf("%q against %q: got %s (%s), want noise", p[0][:len(pre)+4], p[1][:len(pre)+4], got.Kind, got.Why)
						}
					}
				}
			}
		}
	})
}

// A value whose escapes show it is quoted-printable (an "=3D") but whose
// other run holds an escape that decodes to a '%' or a '\', which the
// quoted-printable form leaves as written, does not read as
// quoted-printable text in that run: the two are not read in that form,
// and the signature's change is read raw.
func TestAnEscapeLeftAsWrittenIsNoQuotedPrintable(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 385340565, time.UTC)
	for _, tc := range []struct{ name, rec, live string }{
		{"a tab against '%'", "k=3Dv&v1=09" + sigA[2:], "k=3Dv&v1=25" + sigB[2:]},
		{"'%' against a tab", "k=3Dv&v1=25" + sigA[2:], "k=3Dv&v1=09" + sigB[2:]},
		{"a tab against '\\'", "k=3Dv&v1=09" + sigA[2:], "k=3Dv&v1=5C" + sigB[2:]},
		{"'\\' against a tab", "k=3Dv&v1=5C" + sigA[2:], "k=3Dv&v1=09" + sigB[2:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOne(tc.rec, tc.live, at, nil, nil, nil); got.Kind != KindNoise {
				t.Fatalf("got %s (%s), want noise: its change is a signature's, read raw", got.Kind, got.Why)
			}
		})
	}
}

// A mail that says its body is in another transfer encoding (7bit,
// base64) is no quoted-printable text: a signed header in upper-case hex
// whose first two digits happen to be an escape is read raw.
func TestAMailInAnotherTransferEncodingIsNoQuotedPrintable(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	mail := func(cte, sig string) string {
		return "From: shop@test\r\nX-Signature: v1=" + sig + "\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: " + cte + "\r\n\r\nThank you for your order\r\n"
	}
	for _, tc := range []struct{ name, rec, live string }{
		{"7bit", mail("7bit", "2B"+sigA[2:]), mail("7bit", "41"+sigB[2:])},
		{"base64", mail("base64", "3D"+sigA[2:]), mail("base64", "41"+sigB[2:])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOne(tc.rec, tc.live, at, nil, nil, nil); got.Kind != KindNoise {
				t.Fatalf("got %s (%s), want noise: its change is a signature's, read raw", got.Kind, got.Why)
			}
		})
	}
}

// flowedID is an earlier call of the test whose id flowed from from, as
// recorded, to to, live.
func flowedID(at time.Time, from, to string) []Earlier {
	return []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.id", Noise: "body.id", Type: String, Value: from}, Live: to, Place: "body.id"},
	}}}
}

// Quoted-printable text whose escapes are all of characters it may write
// as themselves (an encoder that escapes '!' for EBCDIC gateways) still
// reads as such where a soft line break shows it is, raw or in a JSON
// string; text whose only escapes are of non-ASCII characters (French
// quotes around an id) reads as such too, and so does text that holds one
// such escape (an '=') anywhere: before a space escaped at a line's end, or
// after an 'A' in the same run of escapes. A mail that says it is reads
// with each of its escapes decoded, a '#' escaped for EBCDIC gateways too.
// The id that flowed from an earlier call is followed, each way.
func TestQuotedPrintableShownBySoftBreakOrNonASCIIEscape(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	mail := func(header, id string) string {
		return "From: shop@test\r\nSubject: Your order\r\n" + header + "Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
			"Order =23" + id + " placed =E2=80=94 thank you\r\n"
	}
	for _, tc := range []struct {
		name string
		of   func(id string) string
	}{
		{"soft breaks, '!' escaped", func(id string) string { return "order " + id[:3] + "=\r\n" + id[3:] + " placed. Thank you=21" }},
		{"soft breaks in a JSON string, '!' escaped", func(id string) string { return "order " + id[:3] + `=\r\n` + id[3:] + " placed. Thank you=21" }},
		{"soft breaks in a JSON string, line feeds only", func(id string) string { return "order " + id[:3] + `=\n` + id[3:] + " placed. Thank you=21" }},
		{"french quotes", func(id string) string { return "Commande =C2=AB" + id + "=C2=BB" }},
		{"an '=' escaped, a space escaped at the line's end", func(id string) string { return "order=3D" + id + " placed=20" }},
		{"an 'A' and an '=' in one run of escapes", func(id string) string { return "x=41=3D" + id }},
		{"a mail that says it is, '#' escaped", func(id string) string { return mail("Content-Type: text/plain; charset=utf-8\r\n", id) }},
		{"a mail that says it is, whole, '#' escaped", func(id string) string { return mail("", id) }},
	} {
		for _, ids := range [][2]string{{"A7K29Q", "B8L30R"}, {"B8L30R", "A7K29Q"}} {
			t.Run(tc.name+", "+ids[0], func(t *testing.T) {
				if got := classifyOne(tc.of(ids[0]), tc.of(ids[1]), at, flowedID(at, ids[0], ids[1]), nil, nil); got.Kind != KindVariable {
					t.Fatalf("got %s (%s), want the id that flowed followed as a variable", got.Kind, got.Why)
				}
			})
		}
	}
}

// A value that reads as quoted-printable text by a soft line break (a line
// that ends in base64 padding reads as one) or by an "=3D" of its own is
// read with every escape decoded, as main reads it: nothing in the two runs
// tells an escape from a signature's first two digits there. So a
// signature in upper-case hex in it whose first two digits read as an
// escape of a '+' in one run ("2B" against "41") is refused, each way: the
// stated limit, a false refusal and no false pass. So, after "v1", is one
// whose first two read as an '=' or as no text in one run ("3D" or "80"
// against "41"): "v1=" is then a run of the base64 alphabet in that run
// only, which does not decode ("sha256=" decodes as base32). Two first
// digits that each read as a letter or a digit ("41" against "35") leave a
// run of the hex alphabet that changed, which is noise. Where nothing shows
// it, it reads as written: noise.
func TestASignatureInAValueThatReadsAsQuotedPrintableReadsEveryEscape(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 385340565, time.UTC)
	for _, tc := range []struct {
		pre string
		v1  bool
	}{
		{"X-A: YWJj=\r\nX-Sig: v1=", true},
		{"X-A: YWJj=\r\nX-Sig: sha256=", false},
		{"X-A: YWJj=\nv1=", true},
		{"k=3Dv&v1=", true},
		{"k=3Dv&sha256=", false},
		{"keyId=3DA94F0C11B2,v1=", true},
	} {
		pre := tc.pre
		t.Run(pre, func(t *testing.T) {
			refused := [][2]string{{"2B", "41"}, {"41", "2B"}}
			if tc.v1 {
				refused = append(refused, [2]string{"3D", "41"}, [2]string{"41", "3D"}, [2]string{"80", "41"}, [2]string{"41", "80"})
			}
			for _, p := range refused {
				got := classifyOne(pre+p[0]+sigA[2:], pre+p[1]+sigB[2:], at, nil, nil, nil)
				if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "a run of the base64 alphabet that does not decode") {
					t.Errorf("%s against %s: got %s (%s), want refused: a run of the base64 alphabet that does not decode", p[0], p[1], got.Kind, got.Why)
				}
			}
			for _, p := range [][2]string{{"41", "35"}, {"35", "41"}} {
				if got := classifyOne(pre+p[0]+sigA[2:], pre+p[1]+sigB[2:], at, nil, nil, nil); got.Kind != KindNoise {
					t.Errorf("%s against %s: got %s (%s), want noise: a run of the hex alphabet changed", p[0], p[1], got.Kind, got.Why)
				}
			}
		})
	}
	for _, pre := range []string{"v1=", "sha256=", "x=21y v1=", "caf=E9 v1="} {
		for _, p := range [][2]string{{"2B", "41"}, {"41", "2B"}, {"3D", "41"}, {"41", "80"}} {
			if got := classifyOne(pre+p[0]+sigA[2:], pre+p[1]+sigB[2:], at, nil, nil, nil); got.Kind != KindNoise {
				t.Errorf("%s%s against %s: got %s (%s), want noise: nothing shows it is quoted-printable", pre, p[0], p[1], got.Kind, got.Why)
			}
		}
	}
}

// Two signatures in standard base64, unpadded (43 characters, each with a
// '+' and a '/' after its first two): without its first two characters
// either is no run that decodes.
const (
	b64A = "P9s2vq+DUT36EAWmlmTPl6OcG/wZPxMCvIKgmDvaYQQ"
	b64B = "mHbGaHE14/+IG7iE81OzKCw31S1JCNAk0Hj1aqHBMdI"
)

// A signature in standard base64 that begins "3D" in one run reads as an
// escaped '=' there: read in the quoted-printable form, it loses its first
// two characters and is no run that decodes. So the two runs are read in
// that form only where both read as quoted-printable text: whole (no '='
// that starts no escape, no escape of a '%' or a '\') and shown (an escape
// of an '=' or of non-ASCII text, or a soft line break). Where either is
// not, the signature's change is read raw, each way: noise. Where both are
// (an "=3D" of the value's own, beside a Latin-1 escape of no text or not),
// it is refused, each way: the stated limit, a false refusal.
func TestABase64SignatureThatBeginsAnEscapedEqualsSignInOneRun(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 385340565, time.UTC)
	for _, tc := range []struct {
		name, pre string
		refused   bool
	}{
		{"the other run shows nothing", "sig=", false},
		{"beside an '=' that starts no escape", "k=3Dv&a=b&sig=", false},
		{"beside an escape of a '%'", "k=3Dv&p=25&sig=", false},
		{"beside an escape of a '\\'", "k=3Dv&p=5C&sig=", false},
		{"after a line that ends in base64 padding, beside an '=' that starts no escape", "X-A: YWJj=\r\nX-Sig: keyId=k1,sig=", false},
		{"beside an escaped '=' of its own", "k=3Dv&sig=", true},
		{"beside an escape that decodes to no text", "k=3Dv&caf=E9&sig=", true},
	} {
		for _, p := range [][2]string{{tc.pre + "3D" + b64A[2:], tc.pre + "41" + b64B[2:]}, {tc.pre + "41" + b64A[2:], tc.pre + "3D" + b64B[2:]}} {
			t.Run(tc.name+", "+p[0][len(tc.pre):len(tc.pre)+2], func(t *testing.T) {
				got := classifyOne(p[0], p[1], at, nil, nil, nil)
				if tc.refused && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, "a run of the base64 alphabet that does not decode")) {
					t.Fatalf("got %s (%s), want refused: a run of the base64 alphabet that does not decode", got.Kind, got.Why)
				}
				if !tc.refused && got.Kind != KindNoise {
					t.Fatalf("got %s (%s), want noise: its change is a signature's, read raw", got.Kind, got.Why)
				}
			})
		}
	}
}

// A payload is read in the quoted-printable form with each of its escapes
// decoded, those of characters quoted-printable may write as themselves
// too: an id an earlier call sent in a mail's body behind a '#' an encoder
// escaped for EBCDIC gateways ("=23A7K29Q") is found there, and the value
// it flowed into is followed as a variable. The control, a payload without
// the id, is not.
func TestAPayloadReadsEachQuotedPrintableEscape(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	sent := func(body string) []Raw {
		m := &models.Mock{Name: "smtp-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: at.Add(-2 * time.Second),
			GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8", Data: "DATA\r\n" + body + "\r\n.\r\n"}}}}}}
		return []Raw{RawOf(m, true)}
	}
	for _, body := range []string{"Order =23ID placed. Thank you=21", "Order=3D=23ID"} {
		if got := classifyOne("A7K29Q", "B8L30R", at, nil, sent(strings.ReplaceAll(body, "ID", "A7K29Q")), nil); got.Kind != KindVariable {
			t.Errorf("%q: got %s (%s), want the id that flowed from the payload followed as a variable", body, got.Kind, got.Why)
		}
		if got := classifyOne("A7K29Q", "B8L30R", at, nil, sent(strings.ReplaceAll(body, "ID", "C9M41S")), nil); got.Kind == KindVariable {
			t.Errorf("%q without the id: got a variable (%s)", body, got.Why)
		}
	}
}

// An id that flowed into a drifted value is followed as the variable it is
// wherever quoted-printable hides it behind an escape of a character it
// may write as itself ("=23" a '#', "=22" a '"', "=20" a space, "=09" a
// tab): the words of the value are searched with each escape decoded,
// whatever shows the value is quoted-printable (a soft line break, raw or
// in a JSON string; an "=3D"; a non-ASCII escape) or that nothing does.
// Only what the learn step checks the value for reads the escapes
// quoted-printable has to write alone (qpFormRead). The id flowed each
// way: from an earlier answer served with its payload (its request or its
// response), or from a recorded payload.
func TestAnIDQuotedPrintableHidesIsFollowed(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	exchange := func(side Side, id string) []Earlier {
		m := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/orders", Body: `{}`, Header: map[string]string{}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: `{}`},
			ReqTimestampMock: at.Add(-time.Second), ResTimestampMock: at.Add(-time.Second + 20*time.Millisecond),
		}}
		if side == Request {
			m.Spec.HTTPReq.Body = `{"id":"` + id + `"}`
		} else {
			m.Spec.HTTPResp.Body = `{"id":"` + id + `"}`
		}
		return []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Payload: RawOf(m, true).ServedAs(1), Seen: []Seen{
			{Side: side, Leaf: Leaf{Path: "body.id", Noise: "body.id", Type: String, Value: id}, Place: "body.id"},
		}}}
	}
	for _, tc := range []struct {
		name string
		of   func(id string) string
	}{
		{"a soft break, '#' escaped", func(id string) string { return "order=\r\n=23" + id + " placed" }},
		{"a soft break in a JSON string, '#' escaped", func(id string) string { return `order=\r\n=23` + id + " placed" }},
		{"an '=' escaped, '#' escaped", func(id string) string { return "x=3D=23" + id }},
		{"an '=' escaped, JSON quotes escaped", func(id string) string { return "q=3D1 =7B=22id=22:=22" + id + "=22=7D" }},
		{"an '=' escaped, a space escaped", func(id string) string { return "a=3Db=20" + id }},
		{"an '=' escaped, a tab escaped", func(id string) string { return "a=3Db=09" + id }},
		{"a non-ASCII escape, '#' escaped", func(id string) string { return "Caf=C3=A9 order =23" + id }},
		{"nothing shown, '#' escaped", func(id string) string { return "Order =23" + id + " placed" }},
	} {
		for _, ids := range [][2]string{{"A7K29QXM", "Zq81Lm0X"}, {"Zq81Lm0X", "A7K29QXM"}, {"b3f2b8c1e9a4d", "f1e2d3c4b5a69"}} {
			rec, live := tc.of(ids[0]), tc.of(ids[1])
			t.Run(tc.name+", "+ids[0], func(t *testing.T) {
				for _, side := range []Side{Request, Response} {
					earlier := exchange(side, ids[0])
					earlier[0].Seen[0].Live = ids[1]
					if got := classifyOne(rec, live, at, earlier, nil, nil); got.Kind != KindVariable {
						t.Errorf("from an earlier %v: got %s (%s), want the id that flowed followed as a variable", side, got.Kind, got.Why)
					}
				}
				raw := []Raw{{rRead: true, mock: "pg-0", at: at.Add(-2 * time.Second), served: true, rText: "INSERT INTO orders (id) VALUES ('" + ids[0] + "')"}}
				if got := classifyOne(rec, live, at, nil, raw, nil); got.Kind != KindVariable {
					t.Errorf("from a recorded payload: got %s (%s), want the id that flowed followed as a variable", got.Kind, got.Why)
				}
				if got := classifyOne(rec, live, at, nil, nil, nil); got.Kind == KindVariable {
					t.Errorf("from nothing: got a variable (%s)", got.Why)
				}
			})
		}
	}
}

// A quoted-printable value in a JSON string is checked with its backslash
// escapes read first, as each other form reads them: they are the JSON
// string's, no stray '\' the learn step cannot read. A drifted id no call
// sent, and a signature in upper-case hex beside an "=3D" whose first two
// digits read as a letter or a digit in each run, are each learned as the
// noise they are, each way.
func TestQuotedPrintableInAJSONStringIsCheckedUnescaped(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	for _, tc := range []struct{ name, rec, live string }{
		{"quotes escaped, an id", `say \"hi\" order=3DA7K29Q`, `say \"hi\" order=3DB8L30R`},
		{"a line feed escaped, an id", `a\nb order=3DA7K29Q`, `a\nb order=3DB8L30R`},
		{"quotes escaped, a signature", `v=3D1 \"sig\":\"v1=41` + sigA[2:] + `\"`, `v=3D1 \"sig\":\"v1=35` + sigB[2:] + `\"`},
	} {
		for _, p := range [][2]string{{tc.rec, tc.live}, {tc.live, tc.rec}} {
			t.Run(tc.name+", "+p[0][len(p[0])-6:], func(t *testing.T) {
				if got := classifyOne(p[0], p[1], at, nil, nil, nil); got.Kind != KindNoise {
					t.Fatalf("got %s (%s), want noise", got.Kind, got.Why)
				}
			})
		}
	}
}

// A mail that says it is quoted-printable is checked with each of its
// escapes decoded, in a JSON string too, whose header may hold a JSON
// escape ("\t" after the colon), or nothing after the colon: the header
// is read with its escapes read.
// A changed run of the standard base64 alphabet that only that reading
// shows ("=2B" a '+') and that does not decode is refused, as in the raw
// mail, each way.
func TestADeclaredMailInAJSONStringIsCheckedAsOne(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	mail := func(nl, tab, tok string) string {
		return "From: shop@test" + nl + "Content-Type: text/plain; charset=utf-8" + nl + "Content-Transfer-Encoding:" + tab + "quoted-printable" + nl + nl + "Token: =2B" + tok + " ok" + nl
	}
	for _, tc := range []struct{ name, nl, tab string }{
		{"raw", "\r\n", "\t"},
		{"in a JSON string", `\r\n`, `\t`},
		{"raw, nothing after the colon", "\r\n", ""},
		{"in a JSON string, nothing after the colon", `\r\n`, ""},
	} {
		for _, p := range [][2]string{{"YmFy", "YmFz"}, {"YmFz", "YmFy"}} {
			t.Run(tc.name+", "+p[0], func(t *testing.T) {
				got := classifyOne(mail(tc.nl, tc.tab, p[0]), mail(tc.nl, tc.tab, p[1]), at, nil, nil, nil)
				if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "a run of the base64 alphabet that does not decode") {
					t.Fatalf("got %s (%s), want refused: a run of the base64 alphabet that does not decode", got.Kind, got.Why)
				}
			})
		}
	}
}

// The form a drifted value is checked in decodes every escape of text
// where the value reads as quoted-printable, those of characters
// quoted-printable may write as themselves too, and leaves one of no text
// (Latin-1) as written, which keeps the value whole. A value whose escapes
// are all of characters quoted-printable may write as themselves, or of no
// text, shows nothing; one with an escape of a '%', or an '=' that starts
// no escape, is not whole.
func TestTheQuotedPrintableFormReadsEveryEscapeOfText(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		qp       bool
	}{
		{"k=3Dv&p=2B=20=3Dx", "k=v&p+ =x", true},
		{"=41=2B=3D", "A+=", true},
		{"=3D=7B=22id=22:=22A7K29Q=22=7D", `={"id":"A7K29Q"}`, true},
		{"caf=E9 =C3=A9t=C3=A9", "caf=E9 \u00e9t\u00e9", true},
		{"x=\r\ny ab=2Bcd", "xy ab+cd", true},
		{"caf=E9 ab=2Bcd", "caf=E9 ab+cd", false},
		{"x=21y ab=2Bcd", "x!y ab+cd", false},
		{"k=3Dv p=25", "k=v p=25", false},
		{"Credential=AKIA/x, Signature=3Dab", "Credential=AKIA/x, Signature=ab", false},
	} {
		if got, qp := qpFormRead(tc.in); got != tc.want || qp != tc.qp {
			t.Errorf("qpFormRead(%q) = %q, %v; want %q, %v", tc.in, got, qp, tc.want, tc.qp)
		}
	}
}

// A signed header that changed only in its signature, beside an earlier
// test of the set that signed with the same scheme, is noise, whatever two
// digits each run's signature begins with: "Signature" stands at its place
// in both runs. The quoted-printable reading of a run whose signature
// begins with an "=XX" that decodes to a letter or a digit ("=41" an 'A',
// "=35" a '5') glues it to the signature ("SignatureAab..."), while the
// other run's "=b1", or "=23" a '#', leaves it a word of its own: that is
// the reading regrouping a word the value holds as written in both runs,
// no word that moved, and the earlier test's header that holds it is no
// place it came from. As an AWS SigV4 header (lower-case hex) signed a
// DynamoDB PutItem in a .NET SDK's record, and a v1= or sha256= value in
// upper-case hex; and a presigned URL, whose percent-escapes are read raw
// and decoded.
func TestAWordAnEscapeGluesInOneRunIsNoWordThatMoved(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 385340565, time.UTC)
	const sigv4 = "AWS4-HMAC-SHA256 Credential=local/20261102/us-east-1/dynamodb/aws4_request, SignedHeaders=content-type;host;x-amz-api-version;x-amz-content-sha256;x-amz-date;x-amz-target, Signature="
	const lowA = "9bbdba4c503009bffa4d5df085bfc642559eb27e81e28586a31d6fa5cf13e5b"
	const lowB = "7da81317308f9dc505ba7b017f223e47ef887b56beb75c5b8d1f34ac6820d9"
	for _, sc := range []struct {
		name, head, digits, a, b string
	}{
		{"aws signature", sigv4, "0123456789abcdef", lowA, lowB},
		{"aws presigned url", "https://shop.s3.amazonaws.com/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAEXAMPLE%2F20261102%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20261102T114804Z&X-Amz-Expires=900&X-Amz-SignedHeaders=host&X-Amz-Signature=", "0123456789abcdef", lowA, lowB},
		{"v1", "t=1730546884,v1=", "0123456789ABCDEF", sigA[2:], sigB[2:]},
		{"sha256", "sha256=", "0123456789ABCDEF", sigA[2:], sigB[2:]},
	} {
		earlier := []Earlier{{Mock: "mock-11", At: at.Add(-2 * time.Second), Run: true, Seen: []Seen{{
			Side: Request, Leaf: Leaf{Path: "header.authorization", Noise: "header.authorization", Type: String, Value: sc.head + "c4" + reversed(sc.a)},
			Live: sc.head + "e7" + reversed(sc.b), Place: "header.authorization",
		}}}}
		t.Run(sc.name, func(t *testing.T) {
			// Against a run whose first two digits are no escape ("b1"), and
			// one whose are an escape of a separator ("23", a '#').
			for _, other := range []string{"b1", "23"} {
				if sc.digits[10] == 'A' {
					other = strings.ToUpper(other)
				}
				for _, hi := range sc.digits {
					for _, lo := range sc.digits {
						xy := string(hi) + string(lo)
						for _, p := range [][2]string{{sc.head + xy + sc.a, sc.head + other + sc.b}, {sc.head + other + sc.a, sc.head + xy + sc.b}} {
							if got := classifyOne(p[0], p[1], at, earlier, nil, nil); got.Kind != KindNoise {
								t.Errorf("%q against %q: got %s (%s), want noise", p[0][len(sc.head):len(sc.head)+4], p[1][len(sc.head):len(sc.head)+4], got.Kind, got.Why)
							}
						}
					}
				}
			}
		})
	}
}

// The quoted-printable form reads a signature that begins "0A" as a line
// break, and so splits the value in that run only: "t=1730546884,v1" and
// the rest. That line stands at its place, as written, in both runs, and
// is no line of the value: an earlier test whose recorded signature also
// begins "0A" holds the same line in its own quoted-printable form, and
// shares no value with this one. A line only that form shows (an id an
// escape hid) is still one (TestIDsAnEscapeHidesThatSwapMoved,
// TestAnIDQuotedPrintableHidesIsFollowed). The heads spell the line in
// lower case, upper case, mixed case and percent-encoded: in each, the
// value holds it as written in one of its forms, decoded or case-folded.
// Percent-encoded in mixed case, the line is held in its own case by the
// decoded form alone.
func TestALineAnEscapeBreaksInOneRunIsNoLineThatFlowed(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 385340565, time.UTC)
	for _, head := range []string{
		"t=1730546884,v1=", "sha256=",
		"X-Sig: t=1730546884,v1=", "Sig: T=1730546884,V1=", "SHA256=",
		"t%3D1730546884%2Cv1=", "X-Sig%3A%20t%3D1730546884%2Cv1=",
	} {
		t.Run(head, func(t *testing.T) {
			for _, earlierLive := range []bool{false, true} {
				// The earlier test's signature that begins "0A", recorded
				// or live.
				erec, elive := head+"0A"+reversed(sigA[2:]), head+"E7"+reversed(sigB[2:])
				if earlierLive {
					erec, elive = elive, erec
				}
				earlier := []Earlier{{Mock: "mock-11", At: at.Add(-2 * time.Second), Run: true, Seen: []Seen{{
					Side: Request, Leaf: Leaf{Path: "header.authorization", Noise: "header.authorization", Type: String, Value: erec},
					Live: elive, Place: "header.authorization",
				}}}}
				const digits = "0123456789ABCDEF"
				for _, hi := range digits {
					for _, lo := range digits {
						xy := string(hi) + string(lo)
						if xy == "0A" {
							continue
						}
						for _, p := range [][2]string{{head + "0A" + sigA[2:], head + xy + sigB[2:]}, {head + xy + sigA[2:], head + "0A" + sigB[2:]}} {
							if got := classifyOne(p[0], p[1], at, earlier, nil, nil); got.Kind != KindNoise {
								t.Errorf("earlier live %v: %q against %q: got %s (%s), want noise", earlierLive, p[0][len(head):len(head)+4], p[1][len(head):len(head)+4], got.Kind, got.Why)
							}
						}
					}
				}
			}
		})
	}
}

// reversed is s back to front.
func reversed(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// Ids an escape hides that changed places are words that moved, in the
// quoted-printable form alone: the value as written holds "23A7K29QXM",
// never "A7K29QXM". Where an earlier test of the set holds one of them,
// recorded or live, at a place that did not drift with it, the value is
// refused, never noise, held in both runs or in one.
func TestIDsAnEscapeHidesThatSwapMoved(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	const a, b = "A7K29QXM", "Zq81Lm0X"
	// Ids in lower case, in a value whose quoted-printable form holds no
	// upper case: that form has no case-folded twin to find them in.
	const c, d = "a7k29qxm", "q81lm0xz"
	for _, tc := range []struct {
		name, rec, live string
		ids             []string
	}{
		{"swapped", "x=3D=23" + a + " y=3D=23" + b, "x=3D=23" + b + " y=3D=23" + a, []string{a, b}},
		{"swapped, in lower case", "x=3D=23" + c + " y=3D=23" + d, "x=3D=23" + d + " y=3D=23" + c, []string{c, d}},
		{"swapped, escaped against as written", "=23" + a + " =23" + b, b + " " + a, []string{a, b}},
		{"swapped, as written against escaped", b + " " + a, "=23" + a + " =23" + b, []string{a, b}},
		{"held once more", "x=3D=23" + a + " y=3D=23" + b, "x=3D=23" + a + " y=3D=23" + b + " z=3D=23" + a, []string{a}},
		{"held once more, recorded", "x=3D=23" + a + " y=3D=23" + b + " z=3D=23" + a, "x=3D=23" + a + " y=3D=23" + b, []string{a}},
	} {
		for _, id := range tc.ids {
			for _, held := range [][2]string{{"ref " + id, "ref " + id}, {"ref " + id, "ref other"}, {"ref other", "ref " + id}} {
				// A word held once more is a word of what changed on the
				// side that holds it once more, searched on that side.
				if strings.HasPrefix(tc.name, "held once more") && (held[1] == "ref other") != strings.HasSuffix(tc.name, "recorded") && held[0] != held[1] {
					continue
				}
				earlier := []Earlier{{Mock: "mock-11", At: at.Add(-2 * time.Second), Run: true, Seen: []Seen{{
					Side: Request, Leaf: Leaf{Path: "body.ref", Noise: "body.ref", Type: String, Value: held[0]}, Live: held[1], Place: "body.ref",
				}}}}
				t.Run(fmt.Sprintf("%s, %s, earlier %q to %q", tc.name, id, held[0], held[1]), func(t *testing.T) {
					if got := classifyOne(tc.rec, tc.live, at, earlier, nil, nil); got.Kind == KindNoise {
						t.Errorf("got noise (%s), want it refused: %s moved, and an earlier test holds it", got.Why, id)
					}
				})
			}
		}
	}
}

// unparsed checks the quoted-printable form where both runs read as one,
// and the text of a value's base64 runs: what only such a form shows, and
// the readings cannot read whole, is refused, each way.
//   - A soft line break joins a run of the standard base64 alphabet that
//     does not decode ("ab+" and "cd"), where both runs are wrapped, where
//     only one is and the other holds no '=', and where its escapes are all
//     of characters quoted-printable may write as themselves ("=21" a '!').
//   - Escapes of non-ASCII text only, which changed ("=C3=A9" an 'é').
//   - A mail that says it is, whose escapes show it is ("=E2=80=94" a
//     dash), is checked with every escape decoded, "=2B" a '+' too.
//   - A '%' in the text a base64 run decodes to.
func TestWhatOnlyACheckedFormShowsIsRefused(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	mail := func(tok string) string {
		return "From: shop@test\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nToken: =2B" + tok + " =E2=80=94 ok\r\n"
	}
	const b64Run, script, escape = "a run of the base64 alphabet that does not decode", "text of a script keploy does not split into words", "a '%' or '\\'"
	for _, tc := range []struct{ name, rec, live, why string }{
		{"wrapped in both runs", "Token: ab+=\r\ncd ok", "Token: ab+=\r\nxy ok", b64Run},
		{"wrapped in one run, no '=' in the other", "Token: ab+=\r\ncd ok", "Token: abzxy ok", b64Run},
		{"wrapped, a '!' escaped", "Token: ab+=\r\ncd ok=21", "Token: ab+=\r\nxy ok=21", b64Run},
		{"wrapped in a JSON string, a '!' escaped", `Token: ab+=\r\ncd ok=21`, `Token: ab+=\r\nxy ok=21`, b64Run},
		{"wrapped in a JSON string in a JSON string, a '!' escaped", `Token: ab+=\\r\\ncd ok=21`, `Token: ab+=\\r\\nxy ok=21`, b64Run},
		{"non-ASCII escapes only", "Commande =C3=A9t=C3=A9 ok", "Commande =C3=A8t=C3=A8 ok", script},
		{"a mail that says it is, its escapes show it", mail("YmFy"), mail("YmFz"), b64Run},
		{"a '%' in base64 text", "data " + b64("price 10% off a7k29q"), "data " + b64("price 10% off b8l30r"), escape},
	} {
		for _, p := range [][2]string{{tc.rec, tc.live}, {tc.live, tc.rec}} {
			t.Run(tc.name+", "+p[0][len(p[0])-6:], func(t *testing.T) {
				if got := classifyOne(p[0], p[1], at, nil, nil, nil); got.Kind != KindUnaccounted || !strings.Contains(got.Why, tc.why) {
					t.Fatalf("got %s (%s), want refused: %s", got.Kind, got.Why, tc.why)
				}
			})
		}
	}
}

// What flowed is followed in every form searched:
//   - an id held in upper case on one side only, flowed from an earlier
//     test in lower case, in the case-folded form of a value that holds
//     upper case in one run only;
//   - ids swapped inside a base64 run's text, which the value also holds as
//     written at their places, in a value with a quoted-printable form: the
//     ids moved in that text, and an earlier test holds one.
func TestWhatFlowedIsFollowedInEveryFormSearched(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	held := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.id", Noise: "body.id", Type: String, Value: "a7k29qxm"}, Live: "q81lm0xz", Place: "body.id"},
	}}}
	for _, p := range [][2]string{{"ref A7K29QXM", "ref q81lm0xz"}, {"ref a7k29qxm", "ref Q81LM0XZ"}} {
		t.Run("case-folded, "+p[0]+", "+p[1], func(t *testing.T) {
			if got := classifyOne(p[0], p[1], at, held, nil, nil); got.Kind != KindVariable {
				t.Fatalf("got %s (%s), want the id that flowed followed as a variable", got.Kind, got.Why)
			}
		})
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	const a, b = "A7K29QXM", "Zq81Lm0X"
	rec := "ref " + a + " " + b + " =23 " + b64(`{"a":"`+a+`","b":"`+b+`"}`)
	live := "ref " + a + " " + b + " =23 " + b64(`{"a":"`+b+`","b":"`+a+`"}`)
	for _, h := range [][2]string{{"ref " + a, "ref " + a}, {"ref " + a, "ref other"}, {"ref other", "ref " + a}} {
		earlier := []Earlier{{Mock: "mock-11", At: at.Add(-2 * time.Second), Run: true, Seen: []Seen{{
			Side: Request, Leaf: Leaf{Path: "body.ref", Noise: "body.ref", Type: String, Value: h[0]}, Live: h[1], Place: "body.ref",
		}}}}
		for _, p := range [][2]string{{rec, live}, {live, rec}} {
			t.Run(fmt.Sprintf("swapped in base64 text, earlier %q to %q", h[0], h[1]), func(t *testing.T) {
				if got := classifyOne(p[0], p[1], at, earlier, nil, nil); got.Kind == KindNoise {
					t.Fatalf("got noise (%s), want it refused: %s moved in the base64 text, and an earlier test holds it", got.Why, a)
				}
			})
		}
	}
}

// A line only the quoted-printable form shows ("order#A7K29Q" of
// "order=23A7K29Q") is a line of the value, and is followed whole where an
// earlier test holds it, though only its punctuation drifted: the value
// as written holds no such line, so nothing written stands for it. Alone,
// and after a line break of its own.
func TestALineOnlyTheQuotedPrintableFormShowsIsFollowedWhole(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	held := payloadEarlier(at, "order#A7K29Q", "order-A7K29Q")
	for _, p := range [][2]string{{"order=23A7K29Q", "order+A7K29Q"}, {"x=0Aorder=23A7K29Q", "x=0Aorder+A7K29Q"}} {
		t.Run(p[0], func(t *testing.T) {
			if got := classifyOne(p[0], p[1], at, held, nil, nil); got.Kind != KindVariable {
				t.Fatalf("got %s (%s), want the line that flowed followed as a variable", got.Kind, got.Why)
			}
		})
	}
}

// A line of the text a base64 run decodes to is a line of the value where
// the value has a quoted-printable form too, though the value also holds
// it as written: only the quoted-printable form's own lines are checked
// against the value as written. Here the text "order#A7K29Q" flowed from
// an earlier test, and only its punctuation drifted.
func TestABase64TextLineIsFollowedBesideAQuotedPrintableForm(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	held := payloadEarlier(at, "order#A7K29Q", "order-A7K29Q")
	rec, live := "k=3D order#A7K29Q "+b64("order#A7K29Q"), "k=3D order+A7K29Q "+b64("order+A7K29Q")
	if got := classifyOne(rec, live, at, held, nil, nil); got.Kind != KindVariable {
		t.Fatalf("got %s (%s), want the line that flowed followed as a variable", got.Kind, got.Why)
	}
}

// A line the quoted-printable form cuts is no line of the value where the
// value holds it as written on its own side, recorded or live, whatever
// the other side holds: "t=1730546884,v1" is the recorded value's own
// text, and the live value, whose separator changed, holds none of it. An
// earlier test whose signature also begins "0A" shares no value with it.
func TestALineHeldAsWrittenOnItsOwnSideIsNoLineThatFlowed(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 385340565, time.UTC)
	comma, semi := "t=1730546884,v1=0A", "t=1730546884;v1=E7"
	for _, p := range [][2]string{{comma + sigA[2:], semi + sigB[2:]}, {semi + sigB[2:], comma + sigA[2:]}} {
		erec, elive := p[0][:len(comma)]+reversed(p[0][len(comma):]), p[1][:len(semi)]+reversed(p[1][len(semi):])
		earlier := []Earlier{{Mock: "mock-11", At: at.Add(-2 * time.Second), Run: true, Seen: []Seen{{
			Side: Request, Leaf: Leaf{Path: "header.authorization", Noise: "header.authorization", Type: String, Value: erec},
			Live: elive, Place: "header.authorization",
		}}}}
		t.Run(p[0][:len(comma)], func(t *testing.T) {
			if got := classifyOne(p[0], p[1], at, earlier, nil, nil); got.Kind != KindNoise {
				t.Fatalf("got %s (%s), want noise", got.Kind, got.Why)
			}
		})
	}
}

// payloadEarlier is an earlier exchange of the test, whose payload keploy
// was given, that sent rec at body.id (live in the full round).
func payloadEarlier(at time.Time, rec, live string) []Earlier {
	t := at.Add(-time.Second)
	m := RawOf(&models.Mock{Name: "mock-1", Spec: models.MockSpec{ReqTimestampMock: t, ResTimestampMock: t.Add(time.Millisecond)}}, true)
	return []Earlier{{Mock: "mock-1", At: t, Own: true, Payload: m.ServedAs(1), Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.id", Noise: "body.id", Type: String, Value: rec}, Live: live, Place: "body.id"},
	}}}
}

// An escape that decodes to no text (a Latin-1 "caf=E9" with no MIME
// header) is still an escape: it is left as written, and the value still
// reads as quoted-printable text where its other escapes or a soft line
// break show it. What only that form shows is then refused, each way, as on
// main: non-ASCII text that changed behind its escapes, and a soft line
// break that joins a run of the base64 alphabet that does not decode. An id
// that flowed behind an escape in such a value is still a variable.
func TestAnEscapeOfNoTextKeepsAValueQuotedPrintable(t *testing.T) {
	at := time.Date(2026, 11, 3, 9, 50, 0, 0, time.UTC)
	for _, tc := range []struct{ name, a, b, why string }{
		{"non-ASCII text that changed", "caf=E9 =C3=A9t=C3=A9 ok", "caf=E9 =C3=A8t=C3=A8 ok", "text of a script keploy does not split into words"},
		{"non-ASCII text that changed, beside a Latin-1 escape that changed", "caf=E9 =C3=A9 ok", "caf=E8 =C3=A8 ok", "text of a script keploy does not split into words"},
		{"a soft line break joins a base64 run", "caf=E9 ab+=\r\ncd ok", "caf=E9 ab+=\r\nce ok", "a run of the base64 alphabet that does not decode"},
	} {
		for _, p := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			t.Run(fmt.Sprintf("%s, %q to %q", tc.name, p[0], p[1]), func(t *testing.T) {
				if got := classifyOne(p[0], p[1], at, nil, nil, nil); got.Kind != KindUnaccounted || !strings.Contains(got.Why, tc.why) {
					t.Fatalf("got %s (%s), want refused: %s", got.Kind, got.Why, tc.why)
				}
			})
		}
	}
	held := payloadEarlier(at, "A7K29Q", "B8L30R")
	for _, p := range [][2]string{{"caf=E9 order =23A7K29Q placed", "caf=E9 order =23B8L30R placed"}, {"caf=E9 =C3=A9 =23A7K29Q", "caf=E9 =C3=A9 =23B8L30R"}} {
		if got := classifyOne(p[0], p[1], at, held, nil, nil); got.Kind != KindVariable {
			t.Errorf("%q to %q: got %s (%s), want the id that flowed followed as a variable", p[0], p[1], got.Kind, got.Why)
		}
	}
}

// A value that reads as quoted-printable text is checked with every escape
// of text decoded, those of characters quoted-printable may write as
// themselves too: an "=2B" there is a '+', and a run of the base64 alphabet
// that changed behind it and does not decode is refused, each way, as main
// refuses it, whatever shows the value is quoted-printable (an "=3D", an
// escape of non-ASCII text, a soft line break, a MIME header), in a
// signature in upper-case hex too. Where nothing shows it (escapes of
// characters it may write as themselves only, or of no text), the value is
// read as written, the form main checks too, which holds no such run: noise.
func TestAnEscapedPlusIsAPlusWhereTheValueReadsAsQuotedPrintable(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, pre, a, b string
		refused         bool
	}{
		{"an escaped '='", "k=3Dv ", "ab=2Bcd ok", "ab=2Bce ok", true},
		{"non-ASCII text", "=C3=A9 ", "ab=2Bcd ok", "ab=2Bce ok", true},
		{"a soft line break", "x=\r\ny ", "ab=2Bcd ok", "ab=2Bce ok", true},
		{"a MIME header", "Content-Transfer-Encoding: quoted-printable\r\n\r\n", "ab=2Bcd ok", "ab=2Bce ok", true},
		{"an escaped '=', a signature", "k=3Dv&", "v1=2B" + sigA[2:], "v1=2B" + sigB[2:], true},
		{"non-ASCII text, a signature", "=C3=A9 ", "v1=2B" + sigA[2:], "v1=2B" + sigB[2:], true},
		{"an escaped '!' only", "x=21y ", "ab=2Bcd ok", "ab=2Bce ok", false},
		{"a Latin-1 escape only", "caf=E9 ", "ab=2Bcd ok", "ab=2Bce ok", false},
		{"nothing, a signature", "", "v1=2B" + sigA[2:], "v1=2B" + sigB[2:], false},
	} {
		for _, p := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			t.Run(fmt.Sprintf("%s, %q", tc.name, p[0][:7]), func(t *testing.T) {
				got := classifyOne(tc.pre+p[0], tc.pre+p[1], at, nil, nil, nil)
				if tc.refused && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, "a run of the base64 alphabet that does not decode")) {
					t.Fatalf("got %s (%s), want refused: a run of the base64 alphabet that does not decode", got.Kind, got.Why)
				}
				if !tc.refused && got.Kind != KindNoise {
					t.Fatalf("got %s (%s), want noise: read as written", got.Kind, got.Why)
				}
			})
		}
	}
}
