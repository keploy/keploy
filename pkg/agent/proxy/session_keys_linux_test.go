package proxy

import (
	"context"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
)

// keysParser reports the session (models.LogKeys) of the context each
// connection it is handed is served under: the one its lines tag their
// calls' paths under.
type keysParser struct {
	saw chan *models.LogKeySession
}

func (keysParser) MatchType(context.Context, []byte) bool { return true }
func (keysParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (keysParser) MockNoise() mocknoise.Adapter { return nil }
func (k keysParser) MockOutgoing(ctx context.Context, _ net.Conn, _ *models.ConditionalDstCfg, _ integrations.MockMemDb, _ models.OutgoingOptions) error {
	k.saw <- models.LogKeys(ctx)
	return nil
}

// Each session the proxy serves tags its connections' calls under the key the
// context its Mock (or Record) was given carries (models.LogKeySession), the
// one its client's route drew as it began: a connection under the next
// session is served under that session's key, never the one before, and a
// session begun with no key serves under the process's (nil). So an agent
// that lives across many sessions names one path by one tag within a session
// and by another in the next.
func TestEachSessionsConnectionsAreTaggedUnderItsOwnKey(t *testing.T) {
	dest := ipv4Dest(t, "127.0.0.1:50071")
	p, _ := instanceProxy(t, dest, nil)
	kp := keysParser{saw: make(chan *models.LogKeySession, 4)}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: kp}
	t.Cleanup(p.endInstances)
	one, two := models.NewLogKeySession(), models.NewLogKeySession()
	for _, s := range []*models.LogKeySession{one, two, nil} {
		if err := p.Mock(models.WithLogKeySession(context.Background(), s), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true}); err != nil {
			t.Fatal(err)
		}
		if got := p.getSession().LogKeys; got != s {
			t.Fatalf("the session holds key %p, want the one its Mock was given, %p", got, s)
		}
		_ = handle(t, p)
		if got := awaitEvent(t, kp.saw, "the parser being handed the connection"); got != s {
			t.Fatalf("the session's connection was served under key %p, want its session's, %p", got, s)
		}
	}
	rec := models.NewLogKeySession()
	if err := p.Record(models.WithLogKeySession(context.Background(), rec), make(chan *models.Mock, 4), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}
	if got := p.getSession().LogKeys; got != rec {
		t.Fatalf("the recording holds key %p, want the one its Record was given, %p", got, rec)
	}
}
