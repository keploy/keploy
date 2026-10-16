package proxy

import (
	"net/netip"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
)

// A MySQL recording made before secrets were left out of fingerprints names
// its container by the legacy key: a live container whose legacy key that
// is, is the recording's, and is said by its current key; one whose legacy
// key is another is not.
func TestAMySQLRecordingOfTheLegacyFormNamesItsContainer(t *testing.T) {
	const legacyKey = "container:mysql:3306@cccccccccccc"
	r := newMysqlPortRegistry()
	r.DeriveFromMocks([]*models.Mock{{Kind: models.MySQL, Spec: models.MockSpec{Metadata: map[string]string{
		containerkey.MetadataKey: legacyKey, containerkey.InstanceKey: "inst", containerkey.ScopeKey: "s1", "destAddr": "10.1.2.3:3306",
	}}}})
	host := netip.MustParseAddr("10.9.9.9")
	why, _, ok := r.recordedEndpoint(host, "container:mysql:3306@sccccccccccc", legacyKey, true, true, true)
	if !ok || why != "the container the recording reached MySQL in (container:mysql:3306@sccccccccccc)" {
		t.Fatalf("the live container of the legacy recording = %q, %v", why, ok)
	}
	if _, _, ok := r.recordedEndpoint(host, "container:mysql:3306@sdddddddddddd", "container:mysql:3306@dddddddddddd", true, true, false); ok {
		t.Fatal("a container whose legacy key is another was taken for the recording's")
	}
}
