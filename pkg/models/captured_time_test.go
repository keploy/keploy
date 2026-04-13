package models

import (
	"context"
	"testing"
	"time"
)

func TestCapturedReqTime_NoSourceFallsBackToNow(t *testing.T) {
	before := time.Now()
	got := CapturedReqTime(context.Background())
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("CapturedReqTime fallback=%v, want within [%v, %v]", got, before, after)
	}
}

func TestCapturedRespTime_NoSourceFallsBackToNow(t *testing.T) {
	before := time.Now()
	got := CapturedRespTime(context.Background())
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("CapturedRespTime fallback=%v, want within [%v, %v]", got, before, after)
	}
}

func TestCapturedReqTime_NilContextFallsBackToNow(t *testing.T) {
	before := time.Now()
	got := CapturedReqTime(nil)
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("CapturedReqTime(nil)=%v, want within [%v, %v]", got, before, after)
	}
}

func TestCapturedReqTime_HonorsContextSource(t *testing.T) {
	want := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	ctx := context.WithValue(context.Background(), CapturedReqTimeKey, func() time.Time { return want })
	got := CapturedReqTime(ctx)
	if !got.Equal(want) {
		t.Fatalf("CapturedReqTime with source = %v, want %v", got, want)
	}
}

func TestCapturedRespTime_HonorsContextSource(t *testing.T) {
	want := time.Date(2026, 4, 13, 12, 30, 0, 0, time.UTC)
	ctx := context.WithValue(context.Background(), CapturedRespTimeKey, func() time.Time { return want })
	got := CapturedRespTime(ctx)
	if !got.Equal(want) {
		t.Fatalf("CapturedRespTime with source = %v, want %v", got, want)
	}
}

func TestCapturedReqTime_RespKeyDoesNotLeakIntoReq(t *testing.T) {
	// Cross-direction guard: if only the resp source is installed,
	// CapturedReqTime must NOT use it — it should fall back to time.Now().
	respTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx := context.WithValue(context.Background(), CapturedRespTimeKey, func() time.Time { return respTime })

	before := time.Now()
	got := CapturedReqTime(ctx)
	after := time.Now()
	if got.Equal(respTime) {
		t.Fatalf("CapturedReqTime leaked the resp-side value (%v); must fall back independently", got)
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("CapturedReqTime fallback=%v, want within [%v, %v]", got, before, after)
	}
}

func TestCapturedReqTime_ZeroFromSourceFallsBackToNow(t *testing.T) {
	// A source that returns time.Time{} (zero) should be treated as
	// no source — the helper must fall back to time.Now() rather than
	// stamp a mock with the zero time, which would break ordering.
	ctx := context.WithValue(context.Background(), CapturedReqTimeKey, func() time.Time { return time.Time{} })

	before := time.Now()
	got := CapturedReqTime(ctx)
	after := time.Now()
	if got.IsZero() {
		t.Fatalf("CapturedReqTime returned zero value; should have fallen back to time.Now()")
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("CapturedReqTime fallback=%v, want within [%v, %v]", got, before, after)
	}
}
