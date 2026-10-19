package http

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
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

// The region, the service, the terminator and the signed headers are
// compared exactly, each named where it differs.
func TestStrictSigV4ComparesTheCredentialScope(t *testing.T) {
	body := "hello"
	rec := recorded("put", "/bucket/key", awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), "20261015T101010Z", hashOf(body)), body)
	for _, tc := range []struct {
		auth string
		want string
	}{
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

// The access key id and the date are never compared (founder ruling on
// SF-3): a replay runs with the credentials of where it runs, a CI's, not
// the recording machine's, and on the day it runs, whatever its signing time
// says. Compared, every replay with other AWS credentials or on another day
// missed.
func TestStrictSigV4NeverComparesTheAccessKeyOrTheDate(t *testing.T) {
	body := "hello"
	auth := func(key, date string, n int) string { return sigV4(key, date, "us-east-1", "s3", signed, sig(n)) }
	rec := recorded("put", "/bucket/key", awsCall(auth("AKID", "20261015", 1), "20261015T235959Z", hashOf(body)), body)
	for _, tc := range []struct {
		name string
		live map[string]string
	}{
		{"other credentials", awsCall(auth("CIKEY", "20261015", 2), "20261015T101533Z", hashOf(body))},
		{"another day", awsCall(auth("AKID", "20261023", 2), "20261023T081500Z", hashOf(body))},
		{"a date that is not its signing day", awsCall(auth("AKID", "20261016", 2), "20261015T101010Z", hashOf(body))},
		{"no signing time", map[string]string{"Authorization": auth("AKID", "20261016", 2), "X-Amz-Content-Sha256": hashOf(body)}},
		{"other credentials on another day", awsCall(auth("CIKEY", "20261101", 2), "20261101T120000Z", hashOf(body))},
	} {
		if got, diag := strictMatch(t, sent("/bucket/key", tc.live, body), nil, nil, nil, rec); got == nil {
			t.Fatalf("%s: not served: differs %v", tc.name, differs(diag))
		}
	}
	// A request dated by its Date header, the same.
	byDate := recorded("put", "/bucket/key", map[string]string{"Authorization": auth("AKID", "20261015", 1), "Date": "Thu, 15 Oct 2026 10:10:10 GMT"}, body)
	if got, diag := strictMatch(t, sent("/bucket/key", map[string]string{"Authorization": auth("CIKEY", "20261016", 2), "Date": "Fri, 16 Oct 2026 08:00:00 GMT"}, body), nil, nil, nil, byDate); got == nil {
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
	// Nor its access key id or its date (SF-3).
	for _, cred := range []string{"OTHER/20261015/us-east-1/s3/aws4_request", "AKID/20261016/us-east-1/s3/aws4_request"} {
		if got, diag := strictMatch(t, sent(presigned(cred, "20261015T101533Z", 900, 2), nil, ""), nil, nil, nil, rec); got == nil {
			t.Fatalf("a URL presigned with credential %s was not served: differs %v", cred, differs(diag))
		}
	}
	for _, tc := range []struct {
		path, want string
	}{
		{presigned("AKID/20261015/eu-west-1/s3/aws4_request", "20261015T101533Z", 900, 2), "query.X-Amz-Credential.region"},
		{presigned("AKID/20261015/us-east-1/sqs/aws4_request", "20261015T101533Z", 900, 2), "query.X-Amz-Credential.service"},
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

// A learned Authorization does not excuse a SigV4 credential's scope or its
// signed headers: a learner that reads the header whole (L, at record's full
// round) sees the signature drift on every SigV4 call and learns
// header.authorization, and the parts are read before that noise. Only the
// signature, the access key id and the date may then differ; with the noise
// checked first, another region or service was served.
func TestStrictSigV4ALearnedAuthorizationKeepsTheScope(t *testing.T) {
	body := "hello"
	rec := recorded("put", "/bucket/key", awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), "20261015T101010Z", hashOf(body)), body)
	rec.Spec.ReqBodyNoise = map[string][]string{"header.authorization": {}}
	if got, diag := strictMatch(t, sent("/bucket/key", awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)), "20261015T101533Z", hashOf(body)), body), nil, nil, nil, rec); got == nil {
		t.Fatalf("a call signed again was not served: differs %v", differs(diag))
	}
	for _, tc := range []struct {
		auth string
		want string
	}{
		{sigV4("AKID", "20261015", "eu-west-1", "s3", signed, sig(2)), "header.authorization.region"},
		{sigV4("AKID", "20261015", "us-east-1", "sqs", signed, sig(2)), "header.authorization.service"},
		{strings.Replace(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)), "aws4_request", "aws5_request", 1), "header.authorization.terminator"},
		{sigV4("AKID", "20261015", "us-east-1", "s3", "host;x-amz-date", sig(2)), "header.authorization.signed_headers"},
		// A live value that is no SigV4 is compared whole: the noise was
		// learned for the recording's signature.
		{"Bearer token", "header.authorization"},
		{strings.Replace(sigV4("OTHER", "20261015", "eu-west-1", "s3", signed, sig(2)), sigV4Algorithm, "AWS4-ECDSA-P256-SHA256", 1), "header.authorization"},
	} {
		got, diag := strictMatch(t, sent("/bucket/key", awsCall(tc.auth, "20261015T101533Z", hashOf(body)), body), nil, nil, nil, rec)
		if got != nil {
			t.Fatalf("%s (%s): a learned Authorization excused it", tc.want, tc.auth)
		}
		if d := differs(diag); len(d) != 1 || d[0] != tc.want {
			t.Fatalf("%s: differs %v", tc.want, d)
		}
	}
	// What is no SigV4 is excused by the learned noise, as before.
	bearer := recorded("m", "/bucket/key", map[string]string{"Authorization": "Bearer token-1"}, "")
	bearer.Spec.ReqBodyNoise = map[string][]string{"header.authorization": {}}
	if got, diag := strictMatch(t, sent("/bucket/key", map[string]string{"Authorization": "Bearer token-2"}, ""), nil, nil, nil, bearer); got == nil {
		t.Fatalf("a learned Bearer token was not excused: differs %v", differs(diag))
	}
}

// decodeHTTP keeps the body as the client sent it before it undoes its
// Content-Encoding, and the payload hash is checked against that: a SigV4
// call whose body the client gzipped is served under the switch.
func TestDecodeHTTPChecksThePayloadHashAgainstTheBodyAsSent(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	body := `{"id":"a-1"}`
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(body))
	_ = w.Close()
	recH := awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), "20261015T101010Z", hashOf(gz.String()))
	recH["Content-Encoding"], recH["Content-Type"] = "gzip", "application/json"
	rec := recorded("put", "/bucket/key", recH, body)
	rec.Spec.HTTPReq.ProtoMajor, rec.Spec.HTTPReq.ProtoMinor = 1, 1
	head := fmt.Sprintf("POST /bucket/key HTTP/1.1\r\nHost: api.example\r\nAuthorization: %s\r\nX-Amz-Date: 20261015T101533Z\r\nX-Amz-Content-Sha256: %s\r\nContent-Encoding: gzip\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n",
		sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)), hashOf(gz.String()), gz.Len())
	app, proxy := tcpPair(t)
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	runDecode(t, proxy, func(ctx context.Context) error {
		return h.decodeHTTP(ctx, append([]byte(head), gz.Bytes()...), proxy, nil, &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
	})
	// The bound only turns a hang into a failure.
	if got := readUntil(app, "\r\n\r\n", time.Minute); !strings.HasPrefix(got, "HTTP/1.1 200") {
		t.Fatalf("a gzipped SigV4 call hashed as sent was answered %q", got)
	}
}
