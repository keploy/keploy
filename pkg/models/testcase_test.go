package models

import (
	"testing"
	"time"
)

// A test case's window is its gRPC request's and response's times for a gRPC
// one, whose HTTP ones are zero, and its HTTP ones for every other: what the
// record route and its hold judge it by.
func TestATestCasesWindowIsItsOwnKindsTimes(t *testing.T) {
	req, resp := time.Unix(100, 0), time.Unix(101, 0)
	for _, tc := range []struct {
		name string
		tc   *TestCase
	}{
		{"gRPC", &TestCase{Kind: GRPC_EXPORT, GrpcReq: GrpcReq{Timestamp: req}, GrpcResp: GrpcResp{Timestamp: resp}}},
		{"HTTP", &TestCase{Kind: HTTP, HTTPReq: HTTPReq{Timestamp: req}, HTTPResp: HTTPResp{Timestamp: resp}}},
	} {
		if gotReq, gotResp := tc.tc.Window(); !gotReq.Equal(req) || !gotResp.Equal(resp) {
			t.Errorf("%s: Window() = %v, %v; want %v, %v", tc.name, gotReq, gotResp, req, resp)
		}
	}
	var none *TestCase
	if r, s := none.Window(); !r.IsZero() || !s.IsZero() {
		t.Errorf("a nil test case's window is %v, %v; want zero", r, s)
	}
}
