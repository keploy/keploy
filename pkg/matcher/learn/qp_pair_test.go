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
