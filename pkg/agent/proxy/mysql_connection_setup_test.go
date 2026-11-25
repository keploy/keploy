package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// mysqlSetupMock is a MySQL query mock recorded at app start, in the first
// test's window, with the lifetime the MySQL recorder stated for it.
func mysqlSetupMock(name, lifetime string) *models.Mock {
	m := exactMock(name, 0, models.LifetimeSession)
	m.Kind = models.MySQL
	m.Spec.Metadata = map[string]string{"type": "mocks", models.MetadataLifetimeKey: lifetime}
	return m
}

// Django asks its connection-setup query once per new connection, so a
// recording holds it once, in the test the app first connected in. Stated a
// session mock by the MySQL recorder (recorder/lifetime.go), it is served to a
// later test that opens a new connection; stated per-test, as it was before
// the recorder took CONVERT_TZ(...) IS NOT NULL for session state, it is that
// first test's alone, and the later test's connection got a MySQL error (the
// app answered 500).
func TestAMySQLConnectionSetupStatedSessionIsServedToALaterTest(t *testing.T) {
	session := mysqlSetupMock("setup-session", models.MetadataLifetimeSession)
	perTest := mysqlSetupMock("setup-per-test", models.MetadataLifetimePerTest)
	own := exactMock("b-own", 10*time.Second, models.LifetimePerTest)

	mm := stagedManager(t, []*models.Mock{perTest, own}, []*models.Mock{session})
	mm.ServeExactTest("B", []*models.Mock{own})

	served, err := mm.GetSessionMocks()
	if err != nil {
		t.Fatal(err)
	}
	if !containsMockNamed(served, "setup-session") {
		t.Fatalf("the session-stated setup query is not served to a later test: %v", exactNames(served))
	}
	if containsMockNamed(served, "setup-per-test") {
		t.Fatalf("a per-test-stated setup query from another test's window is served: %v", exactNames(served))
	}
}
