package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The PAT janitor's lease insert writes expires_at, a typed instant 30
// minutes after its call, and a declared clock names it with a 30 m to
// 30 m 1 ms range at a millisecond. Under the time default the
// background's recording of it is checked by its form alone, so a replayed
// lease 30 minutes on passes whatever its milliseconds; under strict times
// the rule's range checks it.
func TestReplayClocksCheckTheLeasesExpiryByItsFormUnderTheDefault(t *testing.T) {
	lease := models.ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + time.Millisecond, Res: time.Millisecond}
	const field = "body.documents.0.expires_at"
	sources := []models.DeclaredSource{{Rule: "pat-janitor-lease-expiry", Kind: models.DeclaredSourceClock, Field: field, Clock: &lease}}
	call := time.Date(2026, 11, 11, 18, 36, 7, 577e6, time.UTC)
	span := Span{From: call, To: call.Add(3 * time.Millisecond)}
	rec := Leaf{Path: field, Noise: field, Type: Instant, Value: call.Add(30 * time.Minute).Format(time.RFC3339Nano)}
	read := func(p string) (Leaf, bool) {
		if p == field {
			return rec, true
		}
		return Leaf{}, false
	}
	got := ReplayClocks(sources, nil, true, true, read, span)
	if off, ok := got[field]; !ok || off.Form == "" || off.Form != TimeForm(rec) {
		t.Fatalf("under the time default the lease's expiry is %+v (%v); want its form %q", off, ok, TimeForm(rec))
	}
	if off, ok := ReplayClocks(sources, nil, true, false, read, span)[field]; !ok || off.Form != "" || off.Min != lease.Min || off.Max != lease.Max {
		t.Fatalf("under strict times the lease's expiry is checked by %+v (%v); want the rule's range", off, ok)
	}
}
