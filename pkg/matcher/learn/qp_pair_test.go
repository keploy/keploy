package learn

import (
	"testing"
	"time"
)

// A value that holds an '=' no quoted-printable escape starts is no
// quoted-printable text, in either run: the "=XX" its signature happens to
// begin with in one run is no escape. An AWS SigV4 Authorization header, as
// a .NET AWS SDK signed a DynamoDB UpdateItem in each run (the recorded
// signature begins "09", an "=09" that reads as a tab; the live one "b1",
// which reads as nothing), changed only in its signature: the change is the
// signature's, which no reading refuses, never "a run of the base64 alphabet
// that does not decode" that only a misread escape made.
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

// Quoted-printable text whose escapes are all of characters it may write
// as themselves (an encoder that escapes '!' for EBCDIC gateways) still
// reads as such where a soft line break shows it is; and text whose only
// escapes are of non-ASCII characters (French quotes around an id) reads
// as such too: the id that flowed from an earlier call is followed.
func TestQuotedPrintableShownBySoftBreakOrNonASCIIEscape(t *testing.T) {
	at := time.Date(2026, 11, 2, 11, 48, 4, 0, time.UTC)
	earlier := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.id", Noise: "body.id", Type: String, Value: "A7K29Q"}, Live: "B8L30R", Place: "body.id"},
	}}}
	for _, tc := range []struct{ name, rec, live string }{
		{"soft breaks, '!' escaped", "order A7K=\r\n29Q placed. Thank you=21", "order B8L=\r\n30R placed. Thank you=21"},
		{"french quotes", "Commande =C2=ABA7K29Q=C2=BB", "Commande =C2=ABB8L30R=C2=BB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOne(tc.rec, tc.live, at, earlier, nil, nil); got.Kind != KindVariable {
				t.Fatalf("got %s (%s), want the id that flowed followed as a variable", got.Kind, got.Why)
			}
		})
	}
}
