package models

import (
	"testing"
	"time"
)

func TestIsInvertedWindow(t *testing.T) {
	t0 := time.Unix(1000, 0)
	cases := []struct {
		name       string
		start, end time.Time
		want       bool
	}{
		{"in order", t0, t0.Add(time.Second), false},
		{"equal timestamps", t0, t0, false},
		{"zero start", time.Time{}, t0, false},
		{"zero end", t0, time.Time{}, false},
		{"both zero", time.Time{}, time.Time{}, false},
		{"end before start by 1ns", t0, t0.Add(-time.Nanosecond), true},
		{"end before start by 1s", t0.Add(time.Second), t0, true},
	}
	for _, c := range cases {
		if got := IsInvertedWindow(c.start, c.end); got != c.want {
			t.Errorf("%s: IsInvertedWindow(%v, %v) = %v, want %v", c.name, c.start, c.end, got, c.want)
		}
	}
	// Monotonic readings are stripped so wall-clock order governs consistently
	// with timestamps serialized over the stream or persisted to YAML.
	now := time.Now()
	if IsInvertedWindow(now, now.Round(0)) {
		t.Fatal("equal wall-clock time with and without monotonic reading must not be inverted")
	}
	if !IsInvertedWindow(now, now.Add(-time.Millisecond).Round(0)) {
		t.Fatal("earlier wall-clock end without monotonic reading must be inverted")
	}
}

func TestTestCaseWindow(t *testing.T) {
	t0 := time.Unix(1000, 0)
	if s, e := TestCaseWindow(nil); !s.IsZero() || !e.IsZero() {
		t.Fatalf("TestCaseWindow(nil) = [%v, %v], want zeros", s, e)
	}
	httpTC := &TestCase{Kind: HTTP, HTTPReq: HTTPReq{Timestamp: t0}, HTTPResp: HTTPResp{Timestamp: t0.Add(time.Second)}}
	if s, e := TestCaseWindow(httpTC); !s.Equal(t0) || !e.Equal(t0.Add(time.Second)) {
		t.Fatalf("HTTP TestCaseWindow = [%v, %v]", s, e)
	}
	grpcTC := &TestCase{Kind: GRPC_EXPORT, GrpcReq: GrpcReq{Timestamp: t0.Add(2 * time.Second)}, GrpcResp: GrpcResp{Timestamp: t0}}
	if s, e := TestCaseWindow(grpcTC); !s.Equal(t0.Add(2*time.Second)) || !e.Equal(t0) {
		t.Fatalf("gRPC TestCaseWindow = [%v, %v]", s, e)
	}
	if !IsInvertedWindow(TestCaseWindow(grpcTC)) {
		t.Fatal("IsInvertedWindow(TestCaseWindow(grpcTC)) = false, want true")
	}
}
