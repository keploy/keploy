package models

import (
	"testing"
	"time"
)

func TestFormatScopeStartIsMillisecondUTC(t *testing.T) {
	at := time.Date(2026, 10, 12, 18, 41, 43, 361_987_654, time.FixedZone("IST", 5*3600+1800))
	if got := FormatScopeStart(at); got != "2026-10-12T13:11:43.361Z" {
		t.Fatalf("FormatScopeStart = %q", got)
	}
	if got := FormatScopeStart(time.Time{}); got != "" {
		t.Fatalf("zero time must format as \"\", got %q", got)
	}
}

func TestScopeClockBasesPicksTheStartMostOfAScopesMocksCarry(t *testing.T) {
	mk := func(name, start string) *Mock {
		m := &Mock{Name: name}
		StampScopeStart(m, start)
		return m
	}
	pool := []*Mock{
		mk("a1", "2026-10-12T13:00:00.000Z"),
		mk("a2", "2026-10-12T13:00:00.000Z"),
		// a capture at the boundary that carries the neighbour's start
		mk("a3", "2026-10-12T12:59:58.000Z"),
		mk("b1", "2026-10-12T13:00:05.000Z"),
		{Name: "c1"},
	}
	got := ScopeClockBases(map[string][]string{
		"test A": {"a1", "a2", "a3"},
		"test B": {"b1"},
		"test C": {"c1"},
		"test D": nil,
	}, pool)
	if got["test A"] != "2026-10-12T13:00:00.000Z" {
		t.Fatalf("test A: got %q, want the start most of its mocks carry", got["test A"])
	}
	if got["test B"] != "2026-10-12T13:00:05.000Z" {
		t.Fatalf("test B: got %q", got["test B"])
	}
	if _, ok := got["test C"]; ok {
		t.Fatalf("test C has no stamped mock and must be left out, got %q", got["test C"])
	}
	if _, ok := got["test D"]; ok {
		t.Fatal("test D has no mocks and must be left out")
	}
}

func TestScopeClockBasesOfAnUnstampedRecordingIsNil(t *testing.T) {
	if got := ScopeClockBases(map[string][]string{"t": {"m"}}, []*Mock{{Name: "m"}}); got != nil {
		t.Fatalf("a recording made before the stamp must give nil, got %v", got)
	}
}
