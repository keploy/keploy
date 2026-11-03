package learn

import (
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

// Where a value reads as quoted-printable text only by a soft line break (a
// line that ends in base64 padding reads as one) or by an "=3D" of its
// own, an escape of a character quoted-printable may write as itself is
// left as written: a signature in upper-case hex whose first two digits
// happen to be one ("2B" a '+', "41" an 'A', "09" a tab) changes as the
// signature it is, against any first two digits, "3D" and "41" too.
func TestAnEscapeQuotedPrintableMayWriteAsItselfIsLeftAsWritten(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 385340565, time.UTC)
	const digits = "0123456789ABCDEF"
	for _, pre := range []string{
		"X-A: YWJj=\r\nX-Sig: v1=",
		"X-A: YWJj=\r\nX-Sig: sha256=",
		"X-A: YWJj=\nv1=",
		"k=3Dv&v1=",
		"k=3Dv&sha256=",
		"keyId=3DA94F0C11B2,v1=",
	} {
		t.Run(pre, func(t *testing.T) {
			pairs := [][2]string{{"2B", "41"}, {"41", "2B"}, {"2B", "09"}, {"09", "2B"}, {"3D", "41"}, {"41", "3D"}}
			for _, hi := range digits {
				for _, lo := range digits {
					xy := string(hi) + string(lo)
					pairs = append(pairs, [2]string{xy, "3D"}, [2]string{"3D", xy}, [2]string{xy, "41"}, [2]string{"41", xy})
				}
			}
			for _, p := range pairs {
				if got := classifyOne(pre+p[0]+sigA[2:], pre+p[1]+sigB[2:], at, nil, nil, nil); got.Kind != KindNoise {
					t.Errorf("%s against %s: got %s (%s), want noise: its change is a signature's", p[0], p[1], got.Kind, got.Why)
				}
			}
		})
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
// that starts no escape, no escape left as written) and shown (an escape
// quoted-printable has to write, or a soft line break). Where either is
// not, the signature's change is read raw, each way.
func TestABase64SignatureThatBeginsAnEscapedEqualsSignInOneRun(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 385340565, time.UTC)
	for _, tc := range []struct{ name, pre string }{
		{"the other run shows nothing", "sig="},
		{"beside an '=' that starts no escape", "k=3Dv&a=b&sig="},
		{"beside an escape of a '%'", "k=3Dv&p=25&sig="},
		{"beside an escape of a '\\'", "k=3Dv&p=5C&sig="},
		{"beside an escape that decodes to no text", "k=3Dv&caf=E9&sig="},
		{"after a line that ends in base64 padding, beside an '=' that starts no escape", "X-A: YWJj=\r\nX-Sig: keyId=k1,sig="},
	} {
		for _, p := range [][2]string{{tc.pre + "3D" + b64A[2:], tc.pre + "41" + b64B[2:]}, {tc.pre + "41" + b64A[2:], tc.pre + "3D" + b64B[2:]}} {
			t.Run(tc.name+", "+p[0][len(tc.pre):len(tc.pre)+2], func(t *testing.T) {
				if got := classifyOne(p[0], p[1], at, nil, nil, nil); got.Kind != KindNoise {
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
// sent, and a signature in upper-case hex beside an "=3D", are each
// learned as the noise they are, each way.
func TestQuotedPrintableInAJSONStringIsCheckedUnescaped(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	for _, tc := range []struct{ name, rec, live string }{
		{"quotes escaped, an id", `say \"hi\" order=3DA7K29Q`, `say \"hi\" order=3DB8L30R`},
		{"a line feed escaped, an id", `a\nb order=3DA7K29Q`, `a\nb order=3DB8L30R`},
		{"quotes escaped, a signature", `v=3D1 \"sig\":\"v1=2B` + sigA[2:] + `\"`, `v=3D1 \"sig\":\"v1=41` + sigB[2:] + `\"`},
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
// escape ("\t" after the colon): the header is read with its escapes read.
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

// The form a drifted value is checked in leaves each escape of a character
// quoted-printable may write as itself as it was written: its own two
// digits, in their own case, wherever it sits in a run of escapes, after
// another such escape too. Whether a changed run of the base64 alphabet
// decodes depends on those digits ("2B=" reads as base32, "20=" does not).
func TestAnEscapeLeftAsWrittenKeepsItsOwnDigits(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"k=3Dv&p=2B=20=3Dx", "k=v&p=2B=20=x"},
		{"=41=2B=3D", "=41=2B="},
		{"=3D=7B=22id=22:=22A7K29Q=22=7D", "==7B=22id=22:=22A7K29Q=22=7D"},
	} {
		if got, qp := qpFormRead(tc.in); got != tc.want || !qp {
			t.Errorf("qpFormRead(%q) = %q, %v; want %q, true", tc.in, got, qp, tc.want)
		}
	}
}
