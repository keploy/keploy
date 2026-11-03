package wire

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

// The capabilities captured from MariaDB 10.6 and MariaDB Connector/C 3.4.9:
// server 0x81fff7fe with extended 0x1d, client 0x00bfa28c with extended 0x1d,
// CLIENT_MYSQL clear on both.
func TestMetadataFraming(t *testing.T) {
	both := mysql.FramingExtendedMetadata | mysql.FramingMetadataFollows
	for _, tc := range []struct {
		name                 string
		server, client       uint32
		serverExt, clientExt uint32
		recorded             uint32
		want                 mysql.MetadataFraming
	}{
		{"mariadb, connector/c", 0x81fff7fe, 0x00bfa28c, 0x1d, 0x1d, 0, both},
		{"mysql server", 0x81fff7ff, 0x00bfa28c, 0x1d, 0x1d, 0, 0},
		{"mysql client", 0x81fff7fe, 0x00bfa28d, 0x1d, 0x1d, 0, 0},
		{"server offers metadata only", 0x81fff7fe, 0x00bfa28c, 0x08, 0x1d, 0, mysql.FramingExtendedMetadata},
		{"client asks cache only", 0x81fff7fe, 0x00bfa28c, 0x1d, 0x10, 0, mysql.FramingMetadataFollows},
		{"no extended caps", 0x81fff7fe, 0x00bfa28c, 0, 0, 0, 0},
		// Replay frames by the recorded client's caps (PreferRecordedCaps).
		{"recorded client is mysql", 0x81fff7fe, 0x00bfa28c, 0x1d, 0x1d, 0x00bfa28d, 0},
	} {
		d := &DecodeContext{ServerCaps: tc.server, ClientCaps: tc.client, ServerMariaDBCaps: tc.serverExt,
			ClientMariaDBCaps: tc.clientExt, RecordedClientCaps: tc.recorded, PreferRecordedCaps: tc.recorded != 0}
		if got := d.MetadataFraming(); got != tc.want {
			t.Errorf("%s: MetadataFraming() = %d, want %d", tc.name, got, tc.want)
		}
	}
}
