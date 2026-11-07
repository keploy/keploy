package models

import (
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// GrpcReq and GrpcResp keep their Timestamp in BSON as RFC3339Nano text, as
// HTTPReq and HTTPResp do (see http.go). With no codec of its own a time.Time
// is stored as a BSON date: milliseconds, in UTC. A gRPC test case read back
// from Mongo then has both ends of its window up to 1 ms early, so a mock the
// app made in the last millisecond before its response falls out of the
// window (the miss http.go describes), and the case's UTC offset is lost.
//
// Every other field keeps the shape the driver's default gives it: the
// shadow structs inline the type's own fields (grpcReqFields, a copy of the
// type without its methods, so encoding it does not call MarshalBSON again),
// and their Timestamp, at the top level, takes the "timestamp" key over the
// inlined one by the driver's embedding rules, as encoding/json's. A field
// added to GrpcReq or GrpcResp is stored with no change here.
//
// A row written before this codec holds a BSON date, and still reads: at the
// millisecond it was stored at, in UTC, as the driver reads a date.

type grpcReqFields GrpcReq

type grpcReqBSON struct {
	Fields    grpcReqFields `bson:",inline"`
	Timestamp string        `bson:"timestamp"`
}

type grpcReqBSONReader struct {
	Fields    grpcReqFields `bson:",inline"`
	Timestamp bson.RawValue `bson:"timestamp"`
}

// MarshalBSON writes the request with its Timestamp as RFC3339Nano text.
func (r GrpcReq) MarshalBSON() ([]byte, error) {
	return bson.Marshal(grpcReqBSON{Fields: grpcReqFields(r), Timestamp: FormatMockTimestamp(r.Timestamp)})
}

// UnmarshalBSON reads the request, its Timestamp as text or as a BSON date.
// A null request reads as the zero GrpcReq, as the driver's default does: the
// driver passes a null or undefined value here as no bytes.
func (r *GrpcReq) UnmarshalBSON(data []byte) error {
	if len(data) == 0 {
		*r = GrpcReq{}
		return nil
	}
	var raw grpcReqBSONReader
	if err := bson.Unmarshal(data, &raw); err != nil {
		return err
	}
	ts, err := decodeBSONTimestamp(raw.Timestamp)
	if err != nil {
		return fmt.Errorf("GrpcReq.UnmarshalBSON timestamp: %w", err)
	}
	*r = GrpcReq(raw.Fields)
	r.Timestamp = ts
	return nil
}

type grpcRespFields GrpcResp

type grpcRespBSON struct {
	Fields    grpcRespFields `bson:",inline"`
	Timestamp string         `bson:"timestamp"`
}

type grpcRespBSONReader struct {
	Fields    grpcRespFields `bson:",inline"`
	Timestamp bson.RawValue  `bson:"timestamp"`
}

// MarshalBSON writes the response with its Timestamp as RFC3339Nano text.
func (r GrpcResp) MarshalBSON() ([]byte, error) {
	return bson.Marshal(grpcRespBSON{Fields: grpcRespFields(r), Timestamp: FormatMockTimestamp(r.Timestamp)})
}

// UnmarshalBSON reads the response, its Timestamp as text or as a BSON date.
// A null response reads as the zero GrpcResp, as the driver's default does: the
// driver passes a null or undefined value here as no bytes.
func (r *GrpcResp) UnmarshalBSON(data []byte) error {
	if len(data) == 0 {
		*r = GrpcResp{}
		return nil
	}
	var raw grpcRespBSONReader
	if err := bson.Unmarshal(data, &raw); err != nil {
		return err
	}
	ts, err := decodeBSONTimestamp(raw.Timestamp)
	if err != nil {
		return fmt.Errorf("GrpcResp.UnmarshalBSON timestamp: %w", err)
	}
	*r = GrpcResp(raw.Fields)
	r.Timestamp = ts
	return nil
}
