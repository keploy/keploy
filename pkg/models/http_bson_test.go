package models

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The HTTP-timestamp round-trip through MongoDB must preserve nanoseconds.
// BSON DateTime is int64 milliseconds, so the default time.Time encoding
// drops the sub-millisecond tail. MarshalBSON/UnmarshalBSON on HTTPReq and
// HTTPResp serialise Timestamp as RFC3339Nano strings instead.

func TestHTTPReqBSON_RoundTripPreservesNanoseconds(t *testing.T) {
	// Matches the failing test-8 / mock-62 case in
	// FAILURE-ANALYSIS.md: mock landed 7 µs past a millisecond boundary.
	original := HTTPReq{
		Method:     "POST",
		ProtoMajor: 1,
		ProtoMinor: 1,
		URL:        "http://api.staging.keploy.io/cluster/deployment-type/init",
		URLParams:  map[string]string{"a": "b"},
		Header:     map[string]string{"Content-Type": "application/json"},
		Body:       `{"deployment_type":"saas"}`,
		Binary:     "",
		Timestamp:  time.Date(2026, 4, 21, 13, 3, 46, 662007331, time.UTC),
	}

	data, err := bson.Marshal(original)
	if err != nil {
		t.Fatalf("MarshalBSON: %v", err)
	}

	var decoded HTTPReq
	if err := bson.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("UnmarshalBSON: %v", err)
	}

	if !decoded.Timestamp.Equal(original.Timestamp) {
		t.Fatalf("timestamp precision lost: want %s (ns=%d), got %s (ns=%d)",
			original.Timestamp.Format(time.RFC3339Nano), original.Timestamp.Nanosecond(),
			decoded.Timestamp.Format(time.RFC3339Nano), decoded.Timestamp.Nanosecond())
	}
	if decoded.Method != original.Method || decoded.URL != original.URL || decoded.Body != original.Body {
		t.Fatalf("non-timestamp field corrupted: %+v", decoded)
	}
}

func TestHTTPRespBSON_RoundTripPreservesNanoseconds(t *testing.T) {
	original := HTTPResp{
		StatusCode:    200,
		Header:        map[string]string{"Content-Type": "application/json"},
		Body:          `{"ok":true}`,
		StatusMessage: "OK",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Timestamp:     time.Date(2026, 4, 21, 13, 3, 46, 662703152, time.UTC),
	}

	data, err := bson.Marshal(original)
	if err != nil {
		t.Fatalf("MarshalBSON: %v", err)
	}

	var decoded HTTPResp
	if err := bson.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("UnmarshalBSON: %v", err)
	}

	if !decoded.Timestamp.Equal(original.Timestamp) {
		t.Fatalf("timestamp precision lost: want %s, got %s",
			original.Timestamp.Format(time.RFC3339Nano),
			decoded.Timestamp.Format(time.RFC3339Nano))
	}
}

// TestHTTPReqBSON_LegacyDateTimeCompat verifies we can still decode records
// written before MarshalBSON landed — i.e. documents where the timestamp is
// a BSON DateTime rather than an RFC3339Nano string. Without this, upgrading
// the api-server would orphan every previously stored test case.
func TestHTTPReqBSON_LegacyDateTimeCompat(t *testing.T) {
	legacyTS := time.Date(2026, 4, 21, 13, 3, 46, 662000000, time.UTC)

	// Hand-build a BSON document matching the pre-change wire shape:
	// every field serialised by the default codec, timestamp as a DateTime.
	legacyDoc := bson.D{
		{Key: "method", Value: "POST"},
		{Key: "protomajor", Value: int32(1)},
		{Key: "protominor", Value: int32(1)},
		{Key: "url", Value: "http://example.invalid/"},
		{Key: "urlparams", Value: bson.M{}},
		{Key: "header", Value: bson.M{"Content-Type": "application/json"}},
		{Key: "body", Value: `{}`},
		{Key: "bodyref", Value: bson.M{"path": "", "size": int64(0)}},
		{Key: "binary", Value: ""},
		{Key: "form", Value: bson.A{}},
		{Key: "timestamp", Value: legacyTS},
	}
	data, err := bson.Marshal(legacyDoc)
	if err != nil {
		t.Fatalf("build legacy doc: %v", err)
	}

	var decoded HTTPReq
	if err := bson.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("UnmarshalBSON (legacy): %v", err)
	}

	if !decoded.Timestamp.Equal(legacyTS) {
		t.Fatalf("legacy DateTime decode mismatch: want %s, got %s",
			legacyTS.Format(time.RFC3339Nano),
			decoded.Timestamp.Format(time.RFC3339Nano))
	}
	if decoded.Method != "POST" || decoded.URL != "http://example.invalid/" {
		t.Fatalf("legacy non-timestamp fields corrupted: %+v", decoded)
	}
}

func TestHTTPReqBSON_ZeroTimestamp(t *testing.T) {
	original := HTTPReq{Method: "GET", URL: "http://x/"}

	data, err := bson.Marshal(original)
	if err != nil {
		t.Fatalf("MarshalBSON: %v", err)
	}

	var decoded HTTPReq
	if err := bson.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("UnmarshalBSON: %v", err)
	}

	if !decoded.Timestamp.IsZero() {
		t.Fatalf("zero timestamp round-trip changed value: %s", decoded.Timestamp)
	}
	if decoded.Method != "GET" || decoded.URL != "http://x/" {
		t.Fatalf("non-timestamp fields corrupted: %+v", decoded)
	}
}
