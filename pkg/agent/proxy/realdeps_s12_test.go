package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// recordedCall is an HTTP mock as keploy mock record writes it: an
// origin-form URL, the wire's Host (no port for the scheme's default), and
// the address and port the app connected to, which the agent stamps
// (starts.Registry.Stamp).
func recordedCall(path, host, destAddr string) *models.Mock {
	md := map[string]string{"connID": "7"}
	if destAddr != "" {
		md["destAddr"] = destAddr
	}
	return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{Metadata: md,
		HTTPReq: &models.HTTPReq{Method: "GET", URL: path, Header: map[string]string{"Host": host}}}}
}

// Under the proven switch, a recorded mock's destination is the mock.real
// entry its connection was decided by (realdeps.List.Recorded). A recorded
// HTTPS call to a name with no port is decided at the port it connected to,
// never at the port a scheme would suggest; one whose connection was not
// kept decides nothing. Without a list, and without the switch, nothing is
// decided.
func TestARecordedMocksDestinationIsDecidedByItsConnection(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	t.Setenv(provenswitch.Env, "1")
	https := recordedCall("/token", "auth.example.com", "93.184.216.34:443")
	_, ok := p.RealDestination(https)
	require.False(t, ok, "no list")
	require.NoError(t, p.SetRealDeps(context.Background(), []string{"auth.example.com:443", "api.internal:80", "localhost:8080"}))
	for _, tc := range []struct {
		mock *models.Mock
		want string
	}{
		{https, "auth.example.com:443"},
		{recordedCall("/v1/charge", "api.internal", "10.9.0.4:443"), ""},
		{recordedCall("/v1/charge", "api.internal", ""), ""},
		{recordedCall("/token", "auth.example.com", ""), ""},
		{recordedCall("/x", "localhost:8080", ""), "localhost:8080"},
		{&models.Mock{Kind: models.Postgres}, ""},
	} {
		got, ok := p.RealDestination(tc.mock)
		require.Equal(t, tc.want != "", ok, "%+v %v", tc.mock.Spec.HTTPReq, tc.mock.Spec.Metadata)
		require.Equal(t, tc.want, got)
	}
	t.Setenv(provenswitch.Env, "0")
	_, ok = p.RealDestination(https)
	require.False(t, ok, "without the switch nothing is decided")
}
