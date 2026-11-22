package mysql

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// The MySQL integration tells the agent which of its mocks its replay serves
// by copy (replayer.ServedByCopy): without it, every lost handshake and
// COM_QUIT would count as needed, and leave out the rest of its recording.
func TestTheIntegrationRegistersWhatItsReplayServesByCopy(t *testing.T) {
	f, ok := syncMock.ServedByCopyOf(models.MySQL)
	if !ok {
		t.Fatal("no ServedByCopy is registered for MySQL")
	}
	quit := &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{
		Metadata: map[string]string{"type": "mocks"},
		MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{
			Header: &mysql.PacketInfo{Type: mysql.CommandStatusToString(mysql.COM_QUIT)},
		}}},
	}}
	got, gotOK := f(quit)
	want, wantOK := replayer.ServedByCopy(quit)
	if got != want || gotOK != wantOK || !gotOK {
		t.Fatalf("the registered ServedByCopy says %q, %v for a COM_QUIT; replayer.ServedByCopy says %q, %v", got, gotOK, want, wantOK)
	}
}
