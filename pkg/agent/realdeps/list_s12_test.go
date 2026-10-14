package realdeps

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
)

// recordedHTTP is an HTTP mock as keploy's recorder writes it (http.go
// parseFinalHTTP, recordv2.go buildHTTPMock): its URL is the request line's
// origin form ("/token"), never "scheme://host/path"; its Host header is the
// wire's, which a client writes without the scheme's default port. destAddr
// is what the agent stamps on each mock of keploy mock record: the address
// and port the app connected to ("" for none).
func recordedHTTP(path, host, destAddr string) *models.Mock {
	m := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"connID": "7"},
		HTTPReq:  &models.HTTPReq{Method: "GET", URL: path, Header: map[string]string{"Host": host}},
	}}
	if destAddr != "" {
		m.Spec.Metadata["destAddr"] = destAddr
	}
	return m
}

// recordedWire is a mock of a protocol whose recording names no host (a
// Postgres one), with the address the app connected to.
func recordedWire(destAddr string) *models.Mock {
	return &models.Mock{Kind: models.Postgres, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": destAddr}}}
}

// A recorded mock's destination is the mock.real entry the accept would have
// decided its connection by, from what the recording kept of it: the
// connection's address and port (destAddr), its container key, and an HTTP
// request's name. A recording that does not say which port its call went to
// decides nothing, whatever its scheme would suggest: the call stays the
// test's, and is judged.
func TestARecordedMocksDestinationIsTheOneItsConnectionWasDecidedBy(t *testing.T) {
	l, err := Parse([]string{
		"auth.example.com:443",
		"api.internal:80",
		"db.real:8080",
		"localhost:8080",
		"localhost:5432",
		"10.0.4.7:6379",
		"127.0.0.1:9000",
		"db:5432",
		"container:redis",
	})
	require.NoError(t, err)
	keyed := recordedWire("172.20.0.3:6379")
	keyed.Spec.Metadata[containerkey.MetadataKey] = containerkey.Key{Repo: "redis", Port: 6379, Fingerprint: "0123456789ab"}.String()
	for _, tc := range []struct {
		name string
		mock *models.Mock
		want string
	}{
		// The connection's port decides a name with no port of its own.
		{"an HTTPS call to a listed name", recordedHTTP("/token", "auth.example.com", "93.184.216.34:443"), "auth.example.com:443"},
		{"an HTTP call to a listed name", recordedHTTP("/v1/charge", "api.internal", "10.9.0.4:80"), "api.internal:80"},
		// The false pass: api.internal is listed at 80; this call went to 443.
		{"an HTTPS call to a name listed at 80", recordedHTTP("/v1/charge", "api.internal", "10.9.0.4:443"), ""},
		{"an HTTP call to a name listed at 443", recordedHTTP("/token", "auth.example.com", "93.184.216.34:80"), ""},
		// No port kept: nothing is decided, at either scheme's port.
		{"a Host with no port, no connection", recordedHTTP("/token", "auth.example.com", ""), ""},
		{"a Host with no port, no connection, listed at 80", recordedHTTP("/v1/charge", "api.internal", ""), ""},
		{"an absolute https URL, no Host, no connection", &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "https://auth.example.com/token"}}}, ""},
		{"a connection with port 0", recordedHTTP("/token", "auth.example.com", "93.184.216.34:0"), ""},
		// A Host written with its port decides when the connection is not kept.
		{"a Host with its port, no connection", recordedHTTP("/rows", "db.real:8080", ""), "db.real:8080"},
		{"a Host with another port, no connection", recordedHTTP("/rows", "db.real:9090", ""), ""},
		// The connection's port wins over the one the Host is written with.
		{"a Host's port the connection did not use", recordedHTTP("/rows", "db.real:8080", "10.1.2.3:18080"), ""},
		// localhost is an address entry: both loopbacks.
		{"localhost with its port, no connection", recordedHTTP("/x", "localhost:8080", ""), "localhost:8080"},
		{"localhost, connected over IPv4", recordedHTTP("/x", "localhost:8080", "127.0.0.1:8080"), "localhost:8080"},
		{"localhost, connected over IPv6", recordedHTTP("/x", "localhost:8080", "[::1]:8080"), "localhost:8080"},
		{"localhost where only 127.0.0.1 is listed", recordedHTTP("/x", "localhost:9000", ""), ""},
		{"127.0.0.1 where only 127.0.0.1 is listed", recordedHTTP("/x", "localhost:9000", "127.0.0.1:9000"), "127.0.0.1:9000"},
		// An address entry decides by the connection's address, never by an
		// IP literal Host, when the connection is kept.
		{"an IP literal Host with its port, no connection", recordedHTTP("/", "10.0.4.7:6379", ""), "10.0.4.7:6379"},
		{"an IP literal Host the connection did not reach", recordedHTTP("/", "10.0.4.7:6379", "10.0.4.8:6379"), ""},
		// A protocol whose recording names no host: its connection decides
		// an address entry; a name entry it cannot tell.
		{"a Postgres call to a localhost entry", recordedWire("127.0.0.1:5432"), "localhost:5432"},
		{"a Postgres call to a name entry", recordedWire("10.0.0.9:5432"), ""},
		{"a Postgres call kept by no connection", &models.Mock{Kind: models.Postgres}, ""},
		// A container key decides a container entry, whatever the address.
		{"a call to a listed container", keyed, "container:redis"},
		{"no mock", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := l.Recorded(tc.mock)
			require.Equal(t, tc.want != "", ok, "decided %q", got)
			require.Equal(t, tc.want, got)
		})
	}
	var none *List
	_, ok := none.Recorded(recordedHTTP("/rows", "db.real:8080", "10.1.2.3:8080"))
	require.False(t, ok, "no list decides nothing")
}
