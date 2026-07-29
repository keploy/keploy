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
		httpMock("cfg", map[string]string{"type": "config"}, t3),                            // Session -> keep
		httpMock("conn", map[string]string{"type": "connection", "connID": "c1"}, t3),       // Connection -> keep
		httpMock("startup", map[string]string{"type": models.HTTPClient}, t1),               // PerTest, pre-cutoff -> keep
		httpMock("pertest", map[string]string{"type": models.HTTPClient}, t3),               // PerTest, in-window -> DROP
		httpMock("inflight", map[string]string{"type": models.HTTPClient}, t5),              // PerTest, post-pruneBefore -> keep
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
