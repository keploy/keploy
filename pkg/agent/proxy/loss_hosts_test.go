package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// lossesOfKind is p's count of kind, as the agent hands it to keploy's CLI.
func lossesOfKind(t *testing.T, p *Proxy, kind models.CaptureLossKind) models.CaptureLoss {
	t.Helper()
	for _, l := range p.CaptureLosses() {
		if l.Kind == kind {
			return l
		}
	}
	t.Fatalf("no %s loss counted: %+v", kind, p.CaptureLosses())
	return models.CaptureLoss{}
}

// A connection keploy refused because no served test worker made it
// (refuseUnserved) lists its CONNECT target's host, an account-numbered name
// among them, with its tag in the connection's session, the tag that
// connection's lines name it by: keploy's CLI logs it by that shape and tag.
func TestARefusedConnectionsLossListsItsHostWithTheConnectionsTag(t *testing.T) {
	keys := models.NewLogKeySession()
	ctx := models.WithLogKeySession(context.Background(), keys)
	p := &Proxy{logger: zap.NewNop()}
	host := "1234567.suitetalk.api.netsuite.com:443"
	err := p.refuseUnserved(ctx, &agent.NetworkAddress{KernelPid: 42}, host, errors.New("instance 0 is not served"))
	require.Error(t, err)
	loss := lossesOfKind(t, p, models.CaptureLossUnattributed)
	require.True(t, loss.HostsListed)
	require.Equal(t, models.LossHosts{{Name: host, Tag: keys.HostTag(host), At: len("the latest: a connection to ")}}, loss.Hosts)
	text := models.CaptureLosses{loss}.LogText()
	require.Contains(t, text, "a connection to "+keys.HostLogName(host)+", from process 42")
	require.NotContains(t, text, "netsuite")
}

// A TLS handshake a client hung up on (clientTLS.hungUp) lists its CONNECT
// target's host with the tag its lines name it by, charged at once (a client
// process keploy cannot name) or held until the counts are read (one that has
// sent no call through keploy's certificate yet, under the proven switch).
func TestAnAbandonedHandshakesLossListsItsHostWithItsLinesTag(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	keys := models.NewLogKeySession()
	ctx := models.WithLogKeySession(context.Background(), keys)
	for _, tgid := range []uint32{0, 7} {
		p := &Proxy{logger: zap.NewNop()}
		dst := leakHost + ":443"
		(&clientTLS{p: p, ctx: ctx, dst: dst, tgid: tgid}).hungUp("gave a TLS handshake up")
		loss := lossesOfKind(t, p, models.CaptureLossTLSAbandoned)
		require.True(t, loss.HostsListed, "process %d", tgid)
		require.Equal(t, models.LossHosts{{Name: dst, Tag: keys.HostTag(dst), At: len("to ")}}, loss.Hosts, "process %d: the loss does not list the destination with its lines' tag, where its What names it", tgid)
		text := models.CaptureLosses{loss}.NotJudgedLogText()
		require.Contains(t, text, "(to "+keys.HostLogName(dst)+")", "process %d", tgid)
		require.False(t, logleak.Holds(text, logleak.Canary), "process %d: the log form names the host whole: %q", tgid, text)
	}
}

// A parser that panicked on a connection to a destination by its name (a
// CONNECT target's, an upstream's) lists that host with its tag in the
// connection's session (countParserPanic).
func TestAParserPanicsLossListsItsHostWithTheConnectionsTag(t *testing.T) {
	keys := models.NewLogKeySession()
	ctx := models.WithLogKeySession(context.Background(), keys)
	p := &Proxy{logger: zap.NewNop()}
	p.countParserPanic(ctx, util.ErrParserPanicked, integrations.HTTP, "orders-svc:8080")
	loss := lossesOfKind(t, p, models.CaptureLossParserPanic)
	require.True(t, loss.HostsListed)
	require.Equal(t, models.LossHosts{{Name: "orders-svc:8080", Tag: keys.HostTag("orders-svc"), At: len("the " + string(integrations.HTTP) + " parser, on a connection to ")}}, loss.Hosts)
	require.Contains(t, models.CaptureLosses{loss}.LogText(), "on a connection to "+keys.HostLogName("orders-svc:8080"))
}
