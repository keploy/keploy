package containerkey

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The legacy fingerprint is, bit for bit, the one recordings made before
// secrets were left out carry (these were Fingerprint's values then), and
// the current one is of another form, which a key of either form parses.
func TestTheLegacyFingerprintIsTheOneOldRecordingsCarry(t *testing.T) {
	if got := legacyFingerprint(suitePostgres()); got != "1a2340a6e411" {
		t.Fatalf("legacyFingerprint(suite postgres) = %s, want the old recordings' 1a2340a6e411", got)
	}
	if got := legacyFingerprint(Config{}); got != "d4165747ffc1" {
		t.Fatalf("legacyFingerprint(empty) = %s, want d4165747ffc1", got)
	}
	now := Fingerprint(suitePostgres())
	if isLegacyFingerprint(now) || !isFingerprint(now) || now == "s"+legacyFingerprint(suitePostgres()) {
		t.Fatalf("Fingerprint = %s: not of the current form", now)
	}
	for _, fp := range []string{now, "1a2340a6e411"} {
		k, err := Parse("container:postgres:5432@" + fp)
		if err != nil || k.Fingerprint != fp {
			t.Fatalf("Parse(@%s) = %+v, %v", fp, k, err)
		}
	}
}

// A live call is keyed in the current form, whatever the recording
// (StateFromContext, KeyFromContext; and a mock is stamped in it), and each
// recorded key is compared in its own form (SameKey): a current one with the
// live key, a legacy one -- recorded before secrets were left out of
// fingerprints -- with the live container's legacy key
// (LegacyKeyFromContext). So a test set recorded across keploy versions
// (refreshed with what a replay captured) compares every mock as it was
// written.
func TestEachRecordedKeyIsComparedInItsOwnForm(t *testing.T) {
	const fpNew, fpOld = "s111111111111", "222222222222"
	newKey, oldKey := "container:postgres:5432@"+fpNew, "container:postgres:5432@"+fpOld
	check := func(t *testing.T, k *Keyer) {
		t.Helper()
		if got, st := use(k, "c"); got != newKey || st != Keyed {
			t.Fatalf("the live call is keyed %q (%v), want the current form %q", got, st, newKey)
		}
		ctx := WithConn(t.Context(), k, "c")
		if got, _, _ := KeyFromContext(ctx); got != newKey {
			t.Fatalf("KeyFromContext = %q, want %q", got, newKey)
		}
		legacy := LegacyKeyFromContext(ctx)
		if legacy != oldKey {
			t.Fatalf("LegacyKeyFromContext = %q, want %q", legacy, oldKey)
		}
		for _, c := range []struct {
			rec  string
			want bool
		}{
			{newKey, true},
			{oldKey, true},
			{"container:postgres:5432@s999999999999", false},
			{"container:postgres:5432@999999999999", false},
			// A current-form key that happens to read as the legacy key's
			// digits is compared with the current key, never the legacy.
			{"container:postgres:5432@s" + fpOld, false},
			{"", false},
		} {
			if got := SameKey(c.rec, newKey, legacy); got != c.want {
				t.Errorf("SameKey(%q) = %v, want %v", c.rec, got, c.want)
			}
		}
		m := &models.Mock{ConnectionID: "c"}
		k.Stamp(m)
		if got := m.Spec.Metadata[MetadataKey]; got != newKey {
			t.Fatalf("a mock is stamped %q, want the current form %q", got, newKey)
		}
	}
	t.Run("named with its configuration", func(t *testing.T) {
		pg := Container{ID: "pg", Image: "postgres:16", Port: 5432, Fingerprint: fpNew, Legacy: fpOld}
		k := NewKeyer(&fakeIndex{at: map[string]Container{"127.0.0.1:40010": pg}, fps: map[string]string{}, pending: map[string]chan struct{}{}})
		_ = name(t, k, "c", "127.0.0.1:40010")
		check(t, k)
	})
	t.Run("its configuration read after the accept", func(t *testing.T) {
		pg := Container{ID: "pg", Image: "postgres:16", Port: 5432}
		f := &bothFingerprints{fakeIndex: fakeIndex{at: map[string]Container{"127.0.0.1:40010": pg}, fps: map[string]string{"pg": fpNew}, pending: map[string]chan struct{}{}}, legacy: map[string]string{"pg": fpOld}}
		k := NewKeyer(f)
		_ = name(t, k, "c", "127.0.0.1:40010")
		check(t, k)
	})
	if got := LegacyKeyFromContext(t.Context()); got != "" {
		t.Fatalf("a context with no connection has a legacy key %q", got)
	}
}

// bothFingerprints is a fake index that reads a container's legacy
// fingerprint with its current one, as Docker's index does
// (fingerprintsResolver).
type bothFingerprints struct {
	fakeIndex
	legacy map[string]string
}

func (b *bothFingerprints) Fingerprints(ctx context.Context, id string) (string, string, bool) {
	fp, ok := b.fakeIndex.Fingerprint(ctx, id)
	return fp, b.legacy[id], ok
}

// A docker-mode agent's list carries each container's legacy fingerprint
// with its current one, so a replay there compares a legacy recording too.
func TestThePushedListCarriesTheLegacyFingerprint(t *testing.T) {
	b, _ := json.Marshal(SnapshotContainer{ID: "r", Fingerprint: "s111111111111", LegacyFingerprint: "222222222222"})
	var back SnapshotContainer
	if err := json.Unmarshal(b, &back); err != nil || back.LegacyFingerprint != "222222222222" {
		t.Fatalf("round trip: %+v, %v (%s)", back, err, b)
	}
	p := newPushedIndex()
	p.apply(Snapshot{Containers: []SnapshotContainer{{ID: "r", Image: "redis:7", IPs: []string{"172.17.0.9"}, Fingerprint: "s111111111111", LegacyFingerprint: "222222222222"}}})
	if fp, legacy, ok := p.Fingerprints(t.Context(), "r"); !ok || fp != "s111111111111" || legacy != "222222222222" {
		t.Fatalf("Fingerprint = %q, %q, %v", fp, legacy, ok)
	}
	if c, ok, _ := p.Lookup(netip.MustParseAddrPort("172.17.0.9:6379")); !ok || c.Legacy != "222222222222" {
		t.Fatalf("Lookup = %+v, %v", c, ok)
	}
}
