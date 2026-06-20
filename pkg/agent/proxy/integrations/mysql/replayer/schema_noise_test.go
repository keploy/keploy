package replayer

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// RecordedBody pulls the COM_QUERY SQL from the mock's first request, and is
// ok=false for anything that isn't a plaintext query packet.
func TestMySQLNoiseAdapter_RecordedBody(t *testing.T) {
	a := mysqlNoiseAdapter{}

	m := dmlMock("q", "update t set a=1 where id=2", nil)
	if body, ok := a.RecordedBody(m); !ok || string(body) != "update t set a=1 where id=2" {
		t.Fatalf("RecordedBody: ok=%v body=%q", ok, string(body))
	}
	if _, ok := a.RecordedBody(nil); ok {
		t.Errorf("RecordedBody(nil) must be ok=false")
	}
	if _, ok := a.RecordedBody(&models.Mock{Kind: models.MySQL}); ok {
		t.Errorf("RecordedBody must be ok=false for a mock with no MySQL requests")
	}
	// First request is a prepared statement, not a plaintext query.
	m2 := &models.Mock{Kind: models.MySQL}
	m2.Spec.MySQLRequests = []mysql.Request{{
		PacketBundle: mysql.PacketBundle{Message: &mysql.StmtPreparePacket{Query: "x"}},
	}}
	if _, ok := a.RecordedBody(m2); ok {
		t.Errorf("RecordedBody must be ok=false when the first request is not a QueryPacket")
	}
}

// Stored/SetLearned round-trip on the kind-agnostic MockSpec.ReqBodyNoise, and
// MySQL has no value-regex obfuscation concept.
func TestMySQLNoiseAdapter_StoredAndSetNoise(t *testing.T) {
	a := mysqlNoiseAdapter{}
	m := &models.Mock{Kind: models.MySQL}
	if got := a.StoredNoise(m); got != nil {
		t.Errorf("StoredNoise on a fresh mock should be nil, got %v", got)
	}
	want := map[string][]string{"body.set:a#0": {"1"}}
	a.SetLearnedNoise(m, want)
	if got := a.StoredNoise(m); !reflect.DeepEqual(got, want) {
		t.Errorf("SetLearnedNoise/StoredNoise round-trip mismatch: want %v got %v", want, got)
	}
	if a.RecordedValueIsNoise(m) != nil {
		t.Errorf("RecordedValueIsNoise should be nil for MySQL")
	}
}

// Diff carries DETECTION semantics: eligible drift not already known, emitted in
// the shared "body."-prefixed vocabulary; comparable reflects the redacted-
// skeleton gate; already-known keys (root-relative) are subtracted.
func TestMySQLNoiseAdapter_Diff(t *testing.T) {
	a := mysqlNoiseAdapter{}
	rec := []byte("update orders set views=5, updated_at='2026-01-01 12:48:36' where region='north'")
	live := []byte("update orders set views=5, updated_at='2026-06-17 14:00:24' where region='north'")

	drift, comparable := a.Diff(nil, rec, live, nil, nil)
	if !comparable {
		t.Fatalf("Diff: expected comparable=true for structurally-equal queries")
	}
	want := map[string][]string{"body.set:updated_at#0": {"2026-01-01 12:48:36"}}
	if !reflect.DeepEqual(drift, want) {
		t.Fatalf("Diff drift mismatch: want %v got %v", want, drift)
	}

	// An already-known (root-relative) key is subtracted -> no new drift.
	drift, comparable = a.Diff(nil, rec, live, map[string][]string{"set:updated_at#0": {}}, nil)
	if !comparable {
		t.Fatalf("Diff: expected comparable=true")
	}
	if len(drift) != 0 {
		t.Errorf("Diff must subtract already-known noise; got %v", drift)
	}

	// A different skeleton (WHERE column changed) is not comparable.
	if _, comparable := a.Diff(nil,
		[]byte("update t set a=1 where id=1"),
		[]byte("update t set a=1 where ix=1"), nil, nil); comparable {
		t.Errorf("Diff: different skeleton must be comparable=false")
	}
}

// End-to-end through the engine: Detect surfaces body.-prefixed drift, Learn
// merges it onto MockSpec.ReqBodyNoise, and KnownNoise then strips it back to
// the root-relative key the strict matcher consults.
func TestMySQLNoiseAdapter_EngineRoundTrip(t *testing.T) {
	eng := schemanoise.New(mysqlNoiseAdapter{}, true, false)
	m := dmlMock("q", "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='north'", nil)
	live := []byte("update orders set views=5, updated_at='2026-06-17 14:00:24' where region='north'")

	drift, comparable := eng.Detect(m, live, nil)
	if !comparable || len(drift) != 1 {
		t.Fatalf("Detect: comparable=%v drift=%v", comparable, drift)
	}
	if eng.Learn(m, drift) != 1 {
		t.Fatalf("Learn should report 1 newly-added path; stored=%v", m.Spec.ReqBodyNoise)
	}
	if _, ok := m.Spec.ReqBodyNoise["body.set:updated_at#0"]; !ok {
		t.Fatalf("Learn must store body.-prefixed key on MockSpec.ReqBodyNoise; got %v", m.Spec.ReqBodyNoise)
	}
	if _, ok := eng.KnownNoise(m, nil)["set:updated_at#0"]; !ok {
		t.Fatalf("KnownNoise must expose the root-relative key; got %v", eng.KnownNoise(m, nil))
	}
}
