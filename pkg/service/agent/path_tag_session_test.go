package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
)

// keyedProxy is a proxy that notes the session (models.LogKeys) of the
// context each session's Record or Mock was given: the proxy tags its
// connections' calls under it (handleConnection).
type keyedProxy struct {
	*workerPool
	mu   sync.Mutex
	keys []*models.LogKeySession
}

func (p *keyedProxy) note(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, models.LogKeys(ctx))
}

func (p *keyedProxy) Mock(ctx context.Context, opts models.OutgoingOptions) error {
	p.note(ctx)
	return p.workerPool.Mock(ctx, opts)
}

func (p *keyedProxy) Record(ctx context.Context, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	p.note(ctx)
	return p.workerPool.Record(ctx, mocks, opts)
}

func (p *keyedProxy) last() *models.LogKeySession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keys[len(p.keys)-1]
}

// keyedIngress is an ingress that notes the session the agent tells it it
// serves (SetLogKeySession).
type keyedIngress struct {
	coreAgent.IncomingProxy
	keys *models.LogKeySession
}

func (i *keyedIngress) SetLogKeySession(s *models.LogKeySession) { i.keys = s }

// The agent names a path in its log by its shape and a path tag
// (models.PathTag), a keyed hash whose key is drawn afresh for each session
// the agent serves: a recording (GetOutgoing) or a replay's session
// (MockOutgoing). The key rides in the context the proxy's Record or Mock is
// given, so the proxy tags that session's connections under it, and the
// ingress takes its connections under the same key. Within a session one
// method and path has one tag, so the session's lines about it match up; the
// next session gives it another, so the log of an agent that lives across
// many sessions (one docker compose or a kubelet keeps up) does not link one
// path's calls from session to session; and the process's own key, which a
// line with no session names under, is no session's. The console and the
// report print the tag each call carries, made where its line was logged, so
// a new key changes no tag they print. The record log names the processes
// the session's calls come from (starts.Registry) under the same key.
func TestEachSessionTagsPathsUnderAKeyOfItsOwn(t *testing.T) {
	a := newClockAgent(t)
	p := &keyedProxy{workerPool: &workerPool{}}
	a.Proxy = p
	in := &keyedIngress{}
	a.IncomingProxy = in
	ctx := context.Background()
	tag := func(s *models.LogKeySession) string { return s.PathTag("GET", "/v1/users/42") }

	require.NoError(t, a.MockOutgoing(ctx, models.OutgoingOptions{}))
	replay := p.last()
	require.NotNil(t, replay, "the replay's session reached the proxy with no key of its own")
	require.Same(t, replay, in.keys, "the ingress serves the replay's session under another key")
	require.Same(t, replay, starts.Default.LogKeySession(), "the record log names the replay's processes under another key")
	require.Equal(t, tag(replay), tag(replay), "one session gave one path two tags")

	require.NoError(t, a.MockOutgoing(ctx, models.OutgoingOptions{}))
	next := p.last()
	require.Same(t, next, in.keys, "the ingress serves the session under another key")
	require.Same(t, next, starts.Default.LogKeySession(), "the record log names the session's processes under another key")
	require.NotEqual(t, tag(replay), tag(next), "two replay sessions gave a path one tag")

	_, err := a.GetOutgoing(ctx, models.OutgoingOptions{})
	require.NoError(t, err)
	record := p.last()
	require.NotNil(t, record, "the recording reached the proxy with no key of its own")
	require.Same(t, record, in.keys, "the ingress serves the recording under another key")
	require.Same(t, record, starts.Default.LogKeySession(), "the record log names the recording's processes under another key")
	require.NotEqual(t, tag(next), tag(record), "a recording gave a path the tag of the session before it")
	require.NotEqual(t, tag(replay), tag(record), "a recording gave a path the tag of an earlier session")

	process := models.PathTag(ctx, "GET", "/v1/users/42")
	for _, s := range []*models.LogKeySession{replay, next, record} {
		require.NotEqual(t, process, tag(s), "a session tags under the process's key")
	}
}
