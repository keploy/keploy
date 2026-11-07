package models

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A gRPC test case read back from Mongo keeps its request and response
// times to the nanosecond, in their own zone, as an HTTP one does. Before,
// they were BSON dates: milliseconds, in UTC.

var ist = time.FixedZone("IST", 5*3600+1800)

func bsonGrpcReq() GrpcReq {
	return GrpcReq{
		Headers: GrpcHeaders{
			PseudoHeaders:   map[string]string{":path": "/users.Users/Get", ":method": "POST"},
			OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
		},
		Body:               GrpcLengthPrefixedMessage{CompressionFlag: 1, MessageLength: 3, DecodedData: "abc"},
		AdditionalMessages: []GrpcLengthPrefixedMessage{{MessageLength: 1, DecodedData: "d"}},
		Timestamp:          time.Date(2026, 11, 7, 10, 15, 30, 123456789, ist),
	}
}

func bsonGrpcResp() GrpcResp {
	return GrpcResp{
		Headers:    GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{}},
		Body:       GrpcLengthPrefixedMessage{MessageLength: 2, DecodedData: "ok"},
		NoMessages: true,
		Trailers:   GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "0"}},
		Timestamp:  time.Date(2026, 11, 7, 10, 15, 30, 987654321, ist),
	}
}

// sameInstantAndOffset fails unless got is want's instant at want's offset.
func sameInstantAndOffset(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	_, gotOff := got.Zone()
	_, wantOff := want.Zone()
	if !got.Equal(want) || gotOff != wantOff {
		t.Errorf("%s: read back %s, want %s", what, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

// reqWithoutTime and respWithoutTime zero the Timestamp, to compare the other
// fields.
func reqWithoutTime(r GrpcReq) GrpcReq    { r.Timestamp = time.Time{}; return r }
func respWithoutTime(r GrpcResp) GrpcResp { r.Timestamp = time.Time{}; return r }

func TestGrpcBSONKeepsNanosecondsAndZone(t *testing.T) {
	req, resp := bsonGrpcReq(), bsonGrpcResp()

	// As api-server stores a test case (a document holding the TestCase) and
	// as a mock's spec holds them (by pointer).
	type stored struct {
		Data TestCase `bson:"data"`
		Mock MockSpec `bson:"mock"`
	}
	in := stored{
		Data: TestCase{Name: "test-1", Kind: GRPC_EXPORT, GrpcReq: req, GrpcResp: resp},
		Mock: MockSpec{GRPCReq: &req, GRPCResp: &resp},
	}
	data, err := bson.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out stored
	if err := bson.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Mock.GRPCReq == nil || out.Mock.GRPCResp == nil {
		t.Fatalf("mock spec lost its gRPC request or response: %+v", out.Mock)
	}

	for _, c := range []struct {
		where     string
		req       GrpcReq
		resp      GrpcResp
		wantKinds bool
	}{
		{"test case", out.Data.GrpcReq, out.Data.GrpcResp, true},
		{"mock", *out.Mock.GRPCReq, *out.Mock.GRPCResp, false},
	} {
		sameInstantAndOffset(t, c.where+" request", c.req.Timestamp, req.Timestamp)
		sameInstantAndOffset(t, c.where+" response", c.resp.Timestamp, resp.Timestamp)
		if got, want := reqWithoutTime(c.req), reqWithoutTime(req); !reflect.DeepEqual(got, want) {
			t.Errorf("%s request fields: got %+v, want %+v", c.where, got, want)
		}
		if got, want := respWithoutTime(c.resp), respWithoutTime(resp); !reflect.DeepEqual(got, want) {
			t.Errorf("%s response fields: got %+v, want %+v", c.where, got, want)
		}
	}
	if out.Data.Name != "test-1" || out.Data.Kind != GRPC_EXPORT {
		t.Errorf("test case fields: got %q %q", out.Data.Name, out.Data.Kind)
	}
}

// The stored document is the one the driver's default gives, key for key and
// value for value, but for "timestamp": RFC3339Nano text in place of a date.
func TestGrpcBSONShapeIsTheDefaultButTheTimestamp(t *testing.T) {
	for _, c := range []struct {
		what   string
		codec  interface{}
		plain  interface{}
		wantTS string
	}{
		{"request", bsonGrpcReq(), grpcReqFields(bsonGrpcReq()), "2026-11-07T10:15:30.123456789+05:30"},
		{"response", bsonGrpcResp(), grpcRespFields(bsonGrpcResp()), "2026-11-07T10:15:30.987654321+05:30"},
		{"zero request", GrpcReq{}, grpcReqFields{}, ""},
		{"zero response", GrpcResp{}, grpcRespFields{}, ""},
	} {
		got, err := bson.Marshal(c.codec)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.what, err)
		}
		def, err := bson.Marshal(c.plain)
		if err != nil {
			t.Fatalf("%s: default marshal: %v", c.what, err)
		}
		gotEl, err := bson.Raw(got).Elements()
		if err != nil {
			t.Fatal(err)
		}
		defEl, err := bson.Raw(def).Elements()
		if err != nil {
			t.Fatal(err)
		}
		if len(gotEl) != len(defEl) {
			t.Fatalf("%s: %d keys, the default has %d:\n%s\n%s", c.what, len(gotEl), len(defEl), bson.Raw(got), bson.Raw(def))
		}
		for i := range gotEl {
			if gotEl[i].Key() != defEl[i].Key() {
				t.Errorf("%s: key %d is %q, the default's is %q", c.what, i, gotEl[i].Key(), defEl[i].Key())
				continue
			}
			if gotEl[i].Key() == "timestamp" {
				if defEl[i].Value().Type != bson.TypeDateTime {
					t.Errorf("%s: the default timestamp is %v, not a date", c.what, defEl[i].Value().Type)
				}
				if s, ok := gotEl[i].Value().StringValueOK(); !ok || s != c.wantTS {
					t.Errorf("%s: timestamp stored as %v, want the text %q", c.what, gotEl[i].Value(), c.wantTS)
				}
				continue
			}
		}
		// The values, as maps all the way down: a Go map's keys are encoded
		// in no fixed order.
		gotM, defM := asMaps(t, got), asMaps(t, def)
		delete(gotM, "timestamp")
		delete(defM, "timestamp")
		if !reflect.DeepEqual(gotM, defM) {
			t.Errorf("%s: stored as %v, the default as %v", c.what, gotM, defM)
		}
	}
}

func asMaps(t *testing.T, doc []byte) bson.M {
	t.Helper()
	dec := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(doc)))
	dec.DefaultDocumentM()
	var m bson.M
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A row stored before the codec, by the driver's default (a BSON date), reads
// as it read then: at the millisecond, in UTC, every other field kept.
func TestGrpcBSONReadsARowStoredAsADate(t *testing.T) {
	req, resp := bsonGrpcReq(), bsonGrpcResp()
	oldReq, err := bson.Marshal(grpcReqFields(req))
	if err != nil {
		t.Fatal(err)
	}
	oldResp, err := bson.Marshal(grpcRespFields(resp))
	if err != nil {
		t.Fatal(err)
	}

	var gotReq GrpcReq
	if err := bson.Unmarshal(oldReq, &gotReq); err != nil {
		t.Fatalf("request stored as a date: %v", err)
	}
	var gotResp GrpcResp
	if err := bson.Unmarshal(oldResp, &gotResp); err != nil {
		t.Fatalf("response stored as a date: %v", err)
	}
	for _, c := range []struct {
		what      string
		got, want time.Time
	}{
		{"request", gotReq.Timestamp, req.Timestamp.Truncate(time.Millisecond).UTC()},
		{"response", gotResp.Timestamp, resp.Timestamp.Truncate(time.Millisecond).UTC()},
	} {
		if !c.got.Equal(c.want) || c.got.Location() != time.UTC {
			t.Errorf("%s stored as a date read back as %s (%v), want %s in UTC", c.what, c.got.Format(time.RFC3339Nano), c.got.Location(), c.want.Format(time.RFC3339Nano))
		}
	}
	if !reflect.DeepEqual(reqWithoutTime(gotReq), reqWithoutTime(req)) {
		t.Errorf("request fields: got %+v, want %+v", reqWithoutTime(gotReq), reqWithoutTime(req))
	}
	if !reflect.DeepEqual(respWithoutTime(gotResp), respWithoutTime(resp)) {
		t.Errorf("response fields: got %+v, want %+v", respWithoutTime(gotResp), respWithoutTime(resp))
	}

	// A zero time stored as a date (year 1) is still zero.
	zero, err := bson.Marshal(grpcReqFields{})
	if err != nil {
		t.Fatal(err)
	}
	var gotZero GrpcReq
	if err := bson.Unmarshal(zero, &gotZero); err != nil {
		t.Fatal(err)
	}
	if !gotZero.Timestamp.IsZero() {
		t.Errorf("a zero time stored as a date read back as %s", gotZero.Timestamp)
	}
}

func TestGrpcBSONZeroTimestamp(t *testing.T) {
	for _, v := range []interface{}{GrpcReq{}, GrpcResp{}} {
		data, err := bson.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var req GrpcReq
		var resp GrpcResp
		if err := bson.Unmarshal(data, &req); err != nil {
			t.Fatal(err)
		}
		if err := bson.Unmarshal(data, &resp); err != nil {
			t.Fatal(err)
		}
		if !req.Timestamp.IsZero() || !resp.Timestamp.IsZero() {
			t.Errorf("zero time read back as %s / %s", req.Timestamp, resp.Timestamp)
		}
	}
}

// A timestamp that is neither text keploy wrote nor a date fails the read,
// and so does a field the type cannot hold, rather than reading as no time
// or an empty message.
func TestGrpcBSONRefusesWhatItCannotRead(t *testing.T) {
	for _, c := range []struct {
		name      string
		doc       bson.D
		timestamp bool // the error names the timestamp
	}{
		{"text that is no time", bson.D{{Key: "timestamp", Value: "11/07/2026 10:15"}}, true},
		{"a number for a time", bson.D{{Key: "timestamp", Value: int32(7)}}, true},
		{"text for a message", bson.D{{Key: "body", Value: "abc"}, {Key: "timestamp", Value: "2026-11-07T10:15:30Z"}}, false},
	} {
		data, err := bson.Marshal(c.doc)
		if err != nil {
			t.Fatal(err)
		}
		var req GrpcReq
		if err := bson.Unmarshal(data, &req); err == nil {
			t.Errorf("request with %s: read with no error, as %+v", c.name, req)
		} else if c.timestamp && !strings.Contains(err.Error(), "GrpcReq.UnmarshalBSON timestamp") {
			t.Errorf("request with %s: error %q does not name the timestamp", c.name, err)
		}
		var resp GrpcResp
		if err := bson.Unmarshal(data, &resp); err == nil {
			t.Errorf("response with %s: read with no error, as %+v", c.name, resp)
		} else if c.timestamp && !strings.Contains(err.Error(), "GrpcResp.UnmarshalBSON timestamp") {
			t.Errorf("response with %s: error %q does not name the timestamp", c.name, err)
		}
	}
}
