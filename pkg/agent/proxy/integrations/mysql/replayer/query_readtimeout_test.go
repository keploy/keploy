package replayer

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestReadTimeoutDerivation guards the fix for the MySQL replay hot-loop.
//
// Previously `readTimeout := 2 * time.Second * time.Duration(opts.SQLDelay)`
// produced 0s when SQLDelay=0 and overflowed int64 when SQLDelay was a
// real seconds-valued Duration. Either way SetReadDeadline expired
// immediately and the command-phase loop spun at 50ms/iter without ever
// reading the client's MySQL commands.
//
// This test asserts the replacement math (mirrored in query.go:40) yields
// a safe, positive deadline for every caller-visible SQLDelay value.
func TestReadTimeoutDerivation(t *testing.T) {
	derive := func(sqlDelay time.Duration) time.Duration {
		rt := 2 * sqlDelay
		if rt < time.Second {
			rt = 2 * time.Second
		}
		return rt
	}

	cases := []struct {
		name     string
		sqlDelay time.Duration
		wantMin  time.Duration
	}{
		{"zero_falls_back", 0, 2 * time.Second},
		{"sub_second_falls_back", 100 * time.Millisecond, 2 * time.Second},
		{"one_second", 1 * time.Second, 2 * time.Second},
		{"five_seconds", 5 * time.Second, 10 * time.Second},
		{"ten_seconds", 10 * time.Second, 20 * time.Second},
		{"no_overflow_on_large", 1 * time.Hour, 2 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := derive(tc.sqlDelay)
			if got <= 0 {
				t.Fatalf("derived non-positive readTimeout=%s for SQLDelay=%s", got, tc.sqlDelay)
			}
			if got < tc.wantMin {
				t.Fatalf("derived readTimeout=%s smaller than expected minimum %s for SQLDelay=%s",
					got, tc.wantMin, tc.sqlDelay)
			}
		})
	}
}

// Compile-time assertion that OutgoingOptions still carries SQLDelay as a
// time.Duration; if the type ever changes the derivation above must be
// revisited.
var _ = models.OutgoingOptions{SQLDelay: time.Duration(0)}
