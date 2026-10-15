package http

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// sigV4 is an AWS SigV4 Authorization header.
func sigV4(keyID, date, region, service, signedHeaders, signature string) string {
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s", keyID, date, region, service, signedHeaders, signature)
}

// sig is a signature: 64 hex digits, distinct per n.
func sig(n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(n)))
	return hex.EncodeToString(sum[:])
}

func hashOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

const signed = "host;x-amz-content-sha256;x-amz-date"

// awsCall is an S3 PUT signed at amzDate with auth, its payload's hash in
// x-amz-content-sha256.
func awsCall(auth, amzDate, payloadHash string) map[string]string {
	return map[string]string{"Authorization": auth, "X-Amz-Date": amzDate, "X-Amz-Content-Sha256": payloadHash}
}

// Under the switch an AWS SigV4 call is the recorded one when its
// Authorization's parts are, whatever its signature and signing time: the
// signature is over the request and the time it is made, so a replay's is
// never the recording's. Compared whole, every AWS SDK call missed.
func TestStrictSigV4SignatureAndSigningTimeMayDiffer(t *testing.T) {
	body := "hello"
	rec := recorded("put", "/bucket/key", awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), "20261015T101010Z", hashOf(body)), body)
	live := sent("/bucket/key", awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)), "20261015T101533Z", hashOf(body)), body)
	if got, diag := strictMatch(t, live, nil, nil, nil, rec); got == nil {
		t.Fatalf("a call signed again was not served: differs %v", differs(diag))
	}
}

// Every other part of the credential scope, and the signed headers, are
// compared exactly, each named where it differs.
func TestStrictSigV4ComparesTheCredentialScope(t *testing.T) {
	body := "hello"
	rec := recorded("put", "/bucket/key", awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), "20261015T101010Z", hashOf(body)), body)
	for _, tc := range []struct {
		auth string
		want string
	}{
		{sigV4("OTHER", "20261015", "us-east-1", "s3", signed, sig(2)), "header.authorization.access_key_id"},
		{sigV4("AKID", "20261015", "eu-west-1", "s3", signed, sig(2)), "header.authorization.region"},
		{sigV4("AKID", "20261015", "us-east-1", "sqs", signed, sig(2)), "header.authorization.service"},
		{strings.Replace(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)), "aws4_request", "aws5_request", 1), "header.authorization.terminator"},
		{sigV4("AKID", "20261015", "us-east-1", "s3", "host;x-amz-date", sig(2)), "header.authorization.signed_headers"},
	} {
		live := sent("/bucket/key", awsCall(tc.auth, "20261015T101533Z", hashOf(body)), body)
		got, diag := strictMatch(t, live, nil, nil, nil, rec)
		if got != nil {
			t.Fatalf("%s: a call that differs in it was served", tc.want)
		}
		if d := differs(diag); len(d) != 1 || d[0] != tc.want {
			t.Fatalf("%s: differs %v", tc.want, d)
		}
	}
}

// The credential's date is the day the request was signed on, which the
// clock the test runs on decides: it may differ from the recording's only
// when each side's date is the day of its own signing time (X-Amz-Date, else
// Date). Anything else is compared.
func TestStrictSigV4TheDateFollowsTheSigningTime(t *testing.T) {
	body := "hello"
	auth := func(date string, n int) string { return sigV4("AKID", date, "us-east-1", "s3", signed, sig(n)) }
	rec := recorded("put", "/bucket/key", awsCall(auth("20261015", 1), "20261015T235959Z", hashOf(body)), body)

	// A test on the real clock signs on another day.
	if got, diag := strictMatch(t, sent("/bucket/key", awsCall(auth("20261016", 2), "20261016T000003Z", hashOf(body)), body), nil, nil, nil, rec); got == nil {
		t.Fatalf("a call signed on another day was not served: differs %v", differs(diag))
	}
	// A date that is not the request's own signing day is compared.
	got, diag := strictMatch(t, sent("/bucket/key", awsCall(auth("20261016", 2), "20261015T101010Z", hashOf(body)), body), nil, nil, nil, rec)
	if got != nil || !hasField(differs(diag), "header.authorization.date") {
		t.Fatalf("a date that is not the request's signing day: served %v, differs %v", got != nil, differs(diag))
	}
	// A request with no signing time says no day: its date is compared.
	noTime := map[string]string{"Authorization": auth("20261016", 2), "X-Amz-Content-Sha256": hashOf(body)}
	if got, diag := strictMatch(t, sent("/bucket/key", noTime, body), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "header.authorization.date") {
		t.Fatalf("a date with no signing time: served %v, differs %v", got != nil, differs(diag))
	}
	// The Date header is the signing time of a request without X-Amz-Date.
	byDate := recorded("put", "/bucket/key", map[string]string{"Authorization": auth("20261015", 1), "Date": "Thu, 15 Oct 2026 10:10:10 GMT"}, body)
	if got, diag := strictMatch(t, sent("/bucket/key", map[string]string{"Authorization": auth("20261016", 2), "Date": "Fri, 16 Oct 2026 08:00:00 GMT"}, body), nil, nil, nil, byDate); got == nil {
		t.Fatalf("a call dated by its Date header was not served: differs %v", differs(diag))
	}
}

// What does not parse as SigV4 on either side, and every other scheme, is
// compared whole, as before.
func TestStrictSigV4ComparesAnythingElseWhole(t *testing.T) {
	for _, tc := range []struct{ name, rec, live string }{
		{"bearer", "Bearer token-1", "Bearer token-2"},
		{"sigv4a", strings.Replace(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), sigV4Algorithm, "AWS4-ECDSA-P256-SHA256", 1), strings.Replace(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)), sigV4Algorithm, "AWS4-ECDSA-P256-SHA256", 1)},
		{"short signature", strings.Replace(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), sig(1), "abc", 1), sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2))},
		{"another field", sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)) + ", Extra=1", sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)) + ", Extra=1"},
		{"a scope of four parts", strings.Replace(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), "/aws4_request", "", 1), strings.Replace(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)), "/aws4_request", "", 1)},
		{"two lines", sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)) + "\n" + sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(3))},
		{"another scheme live", sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), "Bearer token"},
	} {
		rec := recorded("m", "/bucket/key", map[string]string{"Authorization": tc.rec, "X-Amz-Date": "20261015T101010Z"}, "")
		live := sent("/bucket/key", map[string]string{"X-Amz-Date": "20261015T101533Z"}, "")
		for _, line := range strings.Split(tc.live, "\n") {
			live.header.Add("Authorization", line)
		}
		got, diag := strictMatch(t, live, nil, nil, nil, rec)
		if got != nil {
			t.Fatalf("%s: served", tc.name)
		}
		if d := differs(diag); len(d) != 1 || d[0] != "header.authorization" {
			t.Fatalf("%s: differs %v", tc.name, d)
		}
	}
}

// x-amz-content-sha256 is the hash of the body the client sent when the
// recording's is a hash, so a body field keploy excuses may change it; it is
// the recorded literal otherwise (UNSIGNED-PAYLOAD).
func TestStrictSigV4ThePayloadHashIsTheBodys(t *testing.T) {
	auth := func(n int) string { return sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(n)) }
	call := func(n int, hash, body string) *req {
		h := awsCall(auth(n), "20261015T101533Z", hash)
		h["Content-Type"] = "application/json"
		return sent("/bucket/key", h, body)
	}
	recBody := `{"id":"a-1","n":1}`
	rec := recorded("put", "/bucket/key", awsCall(auth(1), "20261015T101010Z", hashOf(recBody)), recBody)
	rec.Spec.HTTPReq.Header["Content-Type"] = "application/json"

	// A learned field changed the body, and with it its hash.
	learned := *rec
	learned.Spec.ReqBodyNoise = map[string][]string{"body.id": {}}
	liveBody := `{"id":"b-2","n":1}`
	if got, diag := strictMatch(t, call(2, hashOf(liveBody), liveBody), nil, nil, nil, &learned); got == nil {
		t.Fatalf("the hash of a body keploy excuses was not served: differs %v", differs(diag))
	}
	// A hash that is not the sent body's.
	got, diag := strictMatch(t, call(2, hashOf("something else"), recBody), nil, nil, nil, rec)
	if got != nil || !hasField(differs(diag), "header.x-amz-content-sha256") {
		t.Fatalf("a hash that is not the body's: served %v, differs %v", got != nil, differs(diag))
	}
	// The recorded literal.
	unsigned := recorded("put", "/bucket/key", awsCall(auth(1), "20261015T101010Z", "UNSIGNED-PAYLOAD"), recBody)
	unsigned.Spec.HTTPReq.Header["Content-Type"] = "application/json"
	if got, diag := strictMatch(t, call(2, "UNSIGNED-PAYLOAD", recBody), nil, nil, nil, unsigned); got == nil {
		t.Fatalf("UNSIGNED-PAYLOAD was not served: differs %v", differs(diag))
	}
	if got, diag := strictMatch(t, call(2, hashOf(recBody), recBody), nil, nil, nil, unsigned); got != nil || !hasField(differs(diag), "header.x-amz-content-sha256") {
		t.Fatalf("a hash for UNSIGNED-PAYLOAD: served %v, differs %v", got != nil, differs(diag))
	}

	// It is a hash of the body as sent, before keploy undoes its encoding.
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(recBody))
	_ = w.Close()
	encoded := call(2, hashOf(gz.String()), recBody)
	encoded.encoded = gz.Bytes()
	if got, diag := strictMatch(t, encoded, nil, nil, nil, rec); got == nil {
		t.Fatalf("the hash of an encoded body was not served: differs %v", differs(diag))
	}
}

// presigned is a SigV4 presigned URL's query for a GET of /bucket/key.
func presigned(cred, amzDate string, expires, n int) string {
	return fmt.Sprintf("/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=%s&X-Amz-Date=%s&X-Amz-Expires=%d&X-Amz-SignedHeaders=host&X-Amz-Signature=%s&x-id=GetObject",
		strings.ReplaceAll(cred, "/", "%2F"), amzDate, expires, sig(n))
}

// A presigned URL's signing parameters are read the same way: the credential
// by its parts, the signature and the signing time not compared; any other
// parameter as any query value.
func TestStrictSigV4ReadsAPresignedURL(t *testing.T) {
	rec := recorded("get", presigned("AKID/20261015/us-east-1/s3/aws4_request", "20261015T101010Z", 900, 1), nil, "")
	if got, diag := strictMatch(t, sent(presigned("AKID/20261015/us-east-1/s3/aws4_request", "20261015T101533Z", 900, 2), nil, ""), nil, nil, nil, rec); got == nil {
		t.Fatalf("a URL presigned again was not served: differs %v", differs(diag))
	}
	if got, diag := strictMatch(t, sent(presigned("AKID/20261016/us-east-1/s3/aws4_request", "20261016T000003Z", 900, 2), nil, ""), nil, nil, nil, rec); got == nil {
		t.Fatalf("a URL presigned on another day was not served: differs %v", differs(diag))
	}
	for _, tc := range []struct {
		path, want string
	}{
		{presigned("AKID/20261015/eu-west-1/s3/aws4_request", "20261015T101533Z", 900, 2), "query.X-Amz-Credential.region"},
		{presigned("OTHER/20261015/us-east-1/s3/aws4_request", "20261015T101533Z", 900, 2), "query.X-Amz-Credential.access_key_id"},
		{presigned("AKID/20261016/us-east-1/s3/aws4_request", "20261015T101533Z", 900, 2), "query.X-Amz-Credential.date"},
		{presigned("AKID/20261015/us-east-1/s3/aws4_request", "20261015T101533Z", 3600, 2), "query.X-Amz-Expires"},
	} {
		got, diag := strictMatch(t, sent(tc.path, nil, ""), nil, nil, nil, rec)
		if got != nil {
			t.Fatalf("%s: served", tc.want)
		}
		if d := differs(diag); len(d) != 1 || d[0] != tc.want {
			t.Fatalf("%s: differs %v", tc.want, d)
		}
	}
	// A query that is no presigned URL's (no algorithm) is compared value by
	// value.
	plain := recorded("get", "/bucket/key?X-Amz-Signature="+sig(1), nil, "")
	if got, diag := strictMatch(t, sent("/bucket/key?X-Amz-Signature="+sig(2), nil, ""), nil, nil, nil, plain); got != nil || !hasField(differs(diag), "query.X-Amz-Signature") {
		t.Fatalf("a lone X-Amz-Signature: served %v, differs %v", got != nil, differs(diag))
	}
}

// The payload hash leaves the switched static list; without the switch every
// header stays on the static list, as before.
func TestStrictSigV4ThePayloadHashLeavesTheSwitchedList(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	for _, h := range models.PayloadHashHeaders {
		if _, ok := models.ProvenFlakyHeaderNoise()[h]; ok {
			t.Fatalf("%s is still on the switched static list", h)
		}
		if _, ok := flakyHeaderNoise()[h]; ok {
			t.Fatalf("under the switch decode still treats %s as noise", h)
		}
	}
	t.Setenv("KEPLOY_PROVEN", "")
	for _, h := range models.PayloadHashHeaders {
		if _, ok := flakyHeaderNoise()[h]; !ok {
			t.Fatalf("without the switch %s left the static list", h)
		}
	}
}
