package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func strictModeEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KEPLOY_STRICT_MOCK_WINDOW"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// httpMock is encodable by the OSS yaml mapper. Its derived lifetime follows
// Metadata["type"]: "config" -> Session, "connection"(+connID) -> Connection
// (both mode-independent); "HTTP_CLIENT" -> PerTest under strict mode.
func httpMock(name string, meta map[string]string, ts time.Time) *models.Mock {
	meta["name"] = name
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.HTTP,
		Name:    name,
		Spec: models.MockSpec{
			Metadata: meta,
			HTTPReq: &models.HTTPReq{
				Method: "GET", URL: "http://x/y", ProtoMajor: 1, ProtoMinor: 1,
				Header: map[string]string{"Host": "x"},
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header: map[string]string{"Content-Type": "application/json"},
				Body:   `{"ok":1}`,
			},
			ReqTimestampMock: ts,
		},
	}
}

func TestDrainToStartupMocks(t *testing.T) {
	if !strictModeEnabled() {
		t.Skip("per-test lifetime derivation requires KEPLOY_STRICT_MOCK_WINDOW=1 (the auto-replay mode)")
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := base                      // < startupCutoff -> startup-init -> keep
	t3 := base.Add(2 * time.Minute) // in-window       -> drop (per-test)
	t5 := base.Add(4 * time.Minute) // > pruneBefore    -> in-flight  -> keep
	startupCutoff := base.Add(1 * time.Minute)
	pruneBefore := base.Add(3 * time.Minute)

	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")

	mocks := []*models.Mock{
		httpMock("cfg", map[string]string{"type": "config"}, t3),                      // Session -> keep
		httpMock("conn", map[string]string{"type": "connection", "connID": "c1"}, t3), // Connection -> keep
		httpMock("startup", map[string]string{"type": models.HTTPClient}, t1),         // PerTest, pre-cutoff -> keep
		httpMock("pertest", map[string]string{"type": models.HTTPClient}, t3),         // PerTest, in-window -> DROP
		httpMock("inflight", map[string]string{"type": models.HTTPClient}, t5),        // PerTest, post-pruneBefore -> keep
	}
	for _, m := range mocks {
		if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
			t.Fatalf("InsertMock %s: %v", m.Name, err)
		}
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := ys.DrainToStartupMocks(context.Background(), "set-0", pruneBefore, startupCutoff); err != nil {
		t.Fatalf("DrainToStartupMocks: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "set-0", "mocks.yaml"))
	if err != nil {
		t.Fatalf("read mocks.yaml: %v", err)
	}
	content := string(got)

	for _, name := range []string{"cfg", "conn", "startup", "inflight"} {
		if !strings.Contains(content, "name: "+name) {
			t.Errorf("expected %q retained (boot/startup/in-flight), but it was drained", name)
		}
	}
	if strings.Contains(content, "name: pertest") {
		t.Errorf("expected per-test in-window mock %q to be drained, but it is still on disk", "pertest")
	}
}

// TestDrainGobDoesNotPersistDerivedLifetime guards the gob round-trip: the keep
// predicate calls DeriveLifetime (which mutates TestModeInfo.Lifetime and sets
// LifetimeDerived=true), and the gob encoder — unlike yaml/json — does persist
// those fields. If the drain wrote the mutated mock back, the next load would
// short-circuit DeriveLifetime and pin a mode-specific classification. This runs
// in the default (lax) mode, so it also pins the lax invariant that HTTP mocks
// are promoted to Session and kept rather than dropped.
func TestDrainGobDoesNotPersistDerivedLifetime(t *testing.T) {
	if strictModeEnabled() {
		t.Skip("this pins the default lax-mode keep-everything invariant")
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	inWindow := base.Add(2 * time.Minute) // between startupCutoff and pruneBefore
	startupCutoff := base.Add(1 * time.Minute)
	pruneBefore := base.Add(3 * time.Minute)

	dir := t.TempDir()
	setDir := filepath.Join(dir, "set-0")
	if err := os.MkdirAll(setDir, 0755); err != nil {
		t.Fatalf("mkdir set dir: %v", err)
	}
	gobPath := filepath.Join(setDir, "mocks.gob")

	// Per-test-tagged HTTP mock, in-window: kept ONLY via the lax Session
	// promotion (both timestamp clauses miss it), so the keep path runs through
	// DeriveLifetime.
	m := httpMock("lax-http", map[string]string{"type": "mocks"}, inWindow)
	if err := writeGobMocksAtomically(context.Background(), gobPath, []*models.Mock{m}); err != nil {
		t.Fatalf("seed gob: %v", err)
	}

	ys := New(zap.NewNop(), dir, "mocks")
	if err := ys.DrainToStartupMocks(context.Background(), "set-0", pruneBefore, startupCutoff); err != nil {
		t.Fatalf("DrainToStartupMocks: %v", err)
	}

	got, err := readGobMocks(gobPath)
	if err != nil {
		t.Fatalf("read gob back: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the lax-promoted HTTP mock kept, got %d mocks", len(got))
	}
	if got[0].TestModeInfo.LifetimeDerived {
		t.Errorf("drain persisted LifetimeDerived=true into the gob; a later load would skip re-derivation")
	}
	if got[0].TestModeInfo.Lifetime != models.LifetimePerTest {
		t.Errorf("expected persisted Lifetime to stay the zero value (undetermined on disk), got %v", got[0].TestModeInfo.Lifetime)
	}
}
