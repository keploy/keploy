package sigv4

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
)

func TestAuthorizationDiffs(t *testing.T) {
	sig1 := strings.Repeat("a", 64)
	sig2 := strings.Repeat("b", 64)
	rec := "AWS4-HMAC-SHA256 Credential=AKIAOLD/20261124/us-east-1/sqs/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-amz-target, Signature=" + sig1
	liveSame := "AWS4-HMAC-SHA256 Credential=AKIANEW/20261125/us-east-1/sqs/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-amz-security-token;x-amz-target, Signature=" + sig2

	if diffs, ok := AuthorizationDiffs(rec, liveSame); !ok || len(diffs) != 0 {
		t.Fatalf("same scope with rotated key/date/sig/token: ok=%v diffs=%+v, want ok=true len=0", ok, diffs)
	}

	liveWrongRegion := "AWS4-HMAC-SHA256 Credential=AKIANEW/20261125/eu-west-1/sqs/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-amz-target, Signature=" + sig2
	if diffs, ok := AuthorizationDiffs(rec, liveWrongRegion); !ok || len(diffs) != 1 || diffs[0].Path != "header.authorization.region" {
		t.Fatalf("wrong region: ok=%v diffs=%+v", ok, diffs)
	}

	liveWrongService := "AWS4-HMAC-SHA256 Credential=AKIANEW/20261125/us-east-1/s3/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-amz-target, Signature=" + sig2
	if diffs, ok := AuthorizationDiffs(rec, liveWrongService); !ok || len(diffs) != 1 || diffs[0].Path != "header.authorization.service" {
		t.Fatalf("wrong service: ok=%v diffs=%+v", ok, diffs)
	}

	liveWrongHeaders := "AWS4-HMAC-SHA256 Credential=AKIANEW/20261125/us-east-1/sqs/aws4_request, SignedHeaders=host;x-amz-date, Signature=" + sig2
	if diffs, ok := AuthorizationDiffs(rec, liveWrongHeaders); !ok || len(diffs) != 1 || diffs[0].Path != "header.authorization.signed_headers" {
		t.Fatalf("wrong signed headers: ok=%v diffs=%+v", ok, diffs)
	}

	if _, ok := AuthorizationDiffs(rec, "Bearer some-token"); ok {
		t.Fatalf("non-SigV4 live header must return ok=false")
	}
}

func TestSamePayloadHash(t *testing.T) {
	body := []byte(`{"QueueUrl":"https://sqs.us-east-1.amazonaws.com/000/q"}`)
	sum := sha256.Sum256(body)
	liveHash := hex.EncodeToString(sum[:])
	recHash := strings.Repeat("0", 64)

	if !SamePayloadHash(recHash, []string{liveHash}, body) {
		t.Fatalf("matching sha256 of live body rejected")
	}
	if SamePayloadHash(recHash, []string{strings.Repeat("f", 64)}, body) {
		t.Fatalf("mismatched sha256 of live body accepted")
	}
	if !SamePayloadHash("UNSIGNED-PAYLOAD", []string{"UNSIGNED-PAYLOAD"}, body) {
		t.Fatalf("matching literal UNSIGNED-PAYLOAD rejected")
	}
	if SamePayloadHash("UNSIGNED-PAYLOAD", []string{liveHash}, body) {
		t.Fatalf("literal vs hash accepted")
	}
}

func TestPresignedDiffs(t *testing.T) {
	sig1 := strings.Repeat("c", 64)
	sig2 := strings.Repeat("d", 64)
	rec := url.Values{
		AmzAlgorithm:  {Algorithm},
		AmzCredential: {"AKIA1/20261124/us-east-1/s3/aws4_request"},
		AmzDate:       {"20261124T100000Z"},
		AmzSignature:  {sig1},
	}
	live := url.Values{
		AmzAlgorithm:     {Algorithm},
		AmzCredential:    {"AKIA2/20261125/us-east-1/s3/aws4_request"},
		AmzDate:          {"20261125T110000Z"},
		AmzSignature:     {sig2},
		AmzSecurityToken: {"session-tok"},
	}
	by, diffs := PresignedDiffs(rec, live)
	if len(by) != 4 || len(diffs) != 0 {
		t.Fatalf("matching presigned: by=%v diffs=%+v", by, diffs)
	}
	live[AmzCredential] = []string{"AKIA2/20261125/ap-south-1/s3/aws4_request"}
	_, diffs = PresignedDiffs(rec, live)
	if len(diffs) != 1 || diffs[0].Path != "query.X-Amz-Credential.region" {
		t.Fatalf("wrong presigned region: diffs=%+v", diffs)
	}
}
