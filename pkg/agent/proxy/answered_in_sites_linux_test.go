//go:build linux

package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// answering is the store a connection was served from and whether the
// recording answered from it.
type answering struct {
	db       integrations.MockMemDb
	answered bool
}

// answeringParser is the one parser of answeringProxy. Each connection the
// proxy hands it is answered as a replaying parser answers one: from the
// mock store the proxy handed it, by the recording it matched (the next of
// names), which it marks used (MarkMockAsUsed). It writes "answered" back.
type answeringParser struct {
	names chan string
	saw   chan answering
}

func (*answeringParser) MatchType(context.Context, []byte) bool { return true }
func (*answeringParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (*answeringParser) MockNoise() mocknoise.Adapter { return nil }
func (a *answeringParser) MockOutgoing(_ context.Context, conn net.Conn, _ *models.ConditionalDstCfg, db integrations.MockMemDb, _ models.OutgoingOptions) error {
	name := <-a.names
	ok := db.MarkMockAsUsed(*answeringMock(name))
	a.saw <- answering{db: db, answered: ok}
	_, err := io.WriteString(conn, "answered\n")
	return err
}

func answeringMock(name string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.HTTP, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession}}
}

// answeringProxy is a proxy in test mode serving on a port of its own, its
// one parser answering, with no session yet: each connection an application
// makes to it is accepted, sent where dest says (its instance too), and
// handed to the parser.
func answeringProxy(t *testing.T, dest *agent.NetworkAddress) (*Proxy, *answeringParser) {
	t.Helper()
	saved := containerkey.Default
	containerkey.Default = containerkey.NewKeyer(containerAt{addr: "none"})
	t.Cleanup(func() { containerkey.Default = saved })
	cfg := config.New()
	cfg.ProxyPort = 0
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{dest: dest}, cfg)
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	ap := &answeringParser{names: make(chan string, 1), saw: make(chan answering, 1)}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: ap}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
	t.Cleanup(p.endInstances)
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	require.NoError(t, <-ready)
	return p, ap
}

// call is an application's call through p that the recording name answers:
// it returns the store p served it from, once the answer is back.
func (a *answeringParser) call(t *testing.T, p *Proxy, name string) integrations.MockMemDb {
	t.Helper()
	a.names <- name
	app, err := net.Dial("tcp", p.Listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = app.Close() }()
	_, err = io.WriteString(app, "GET / HTTP/1.1\r\nHost: dependency\r\n\r\n")
	require.NoError(t, err)
	got := awaitEvent(t, a.saw, "the parser answering "+name)
	require.True(t, got.answered, "%s was not marked used", name)
	line, err := bufio.NewReader(app).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "answered\n", line)
	return got.db
}

// Each mock manager the proxy makes, by a replay session (Mock), by a
// recording session a replay then reuses (Record), and for a test worker
// (ServeInstance), serves under the test windows the agent holds open
// (agent.TestWindowNoter, as Agent.BeginScope tells it): an application's
// call through the proxy that a recording answers while window 1 is open is
// read as answered in it (models.MockState.AnsweredIn), and one after it
// closed as answered outside every window. A manager that served under its
// own windows would read every answer as outside, every test line would say
// it consumed nothing, and the judge would keep an edit whose moved
// recording answered.
func TestEachManagerAProxyMakesServesUnderTheWindowsTheAgentHoldsOpen(t *testing.T) {
	ctx := context.Background()
	opts := models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true}
	stage := func(t *testing.T, p *Proxy) {
		t.Helper()
		require.NoError(t, p.SetMocks(ctx, nil, []*models.Mock{answeringMock("mock-in"), answeringMock("mock-out")}))
	}
	// check reads m (the agent's consumed read for the run's manager): what
	// answered while window 1 was open is read under it, what answered
	// after it closed outside every window.
	check := func(t *testing.T, got []models.MockState) {
		t.Helper()
		by := consumedByName(got)
		require.Len(t, by, 2, "consumed %v", got)
		require.Equal(t, []uint64{1}, seqsOf(by["mock-in"].AnsweredIn), "the answer in window 1 is not read under it")
		require.Zero(t, by["mock-in"].AnsweredOutsideAt)
		require.Empty(t, by["mock-out"].AnsweredIn)
		require.NotZero(t, by["mock-out"].AnsweredOutsideAt, "the answer after window 1 closed is not read as outside every window")
	}
	for _, sw := range []string{"", "1"} {
		t.Run("switch="+sw, func(t *testing.T) {
			t.Run("a replay session's manager", func(t *testing.T) {
				t.Setenv(provenswitch.Env, sw)
				p, ap := answeringProxy(t, ipv4Dest(t, "127.0.0.1:50071"))
				// The window opens before the session makes its manager.
				p.OpenTestWindow(1)
				require.NoError(t, p.Mock(ctx, opts))
				stage(t, p)
				run := p.getMockManager()
				require.NotNil(t, run)
				require.Equal(t, integrations.MockMemDb(run), ap.call(t, p, "mock-in"))
				p.CloseTestWindow(1)
				ap.call(t, p, "mock-out")
				got, err := p.GetConsumedMocks(ctx)
				require.NoError(t, err)
				check(t, got)
			})
			t.Run("a recording session's manager a replay reuses", func(t *testing.T) {
				t.Setenv(provenswitch.Env, sw)
				p, ap := answeringProxy(t, ipv4Dest(t, "127.0.0.1:50072"))
				require.Nil(t, p.getMockManager())
				require.NoError(t, p.Record(ctx, make(chan *models.Mock, 16), opts))
				made := p.getMockManager()
				require.NotNil(t, made, "the recording session made no manager")
				require.NoError(t, p.Mock(ctx, opts))
				require.Same(t, made, p.getMockManager(), "the replay did not reuse the recording's manager")
				stage(t, p)
				p.OpenTestWindow(1)
				require.Equal(t, integrations.MockMemDb(made), ap.call(t, p, "mock-in"))
				p.CloseTestWindow(1)
				ap.call(t, p, "mock-out")
				got, err := p.GetConsumedMocks(ctx)
				require.NoError(t, err)
				check(t, got)
			})
			t.Run("a test worker's manager", func(t *testing.T) {
				t.Setenv(provenswitch.Env, sw)
				dest := ipv4Dest(t, "127.0.0.1:50073")
				dest.KernelPid, dest.Instance = uint32(os.Getpid()), 1
				p, ap := answeringProxy(t, dest)
				require.NoError(t, p.Mock(ctx, opts))
				stage(t, p)
				p.OpenTestWindow(1)
				_, err := p.ServeInstance(1)
				require.NoError(t, err)
				worker := instanceMgrOf(t, p, 1)
				require.Equal(t, integrations.MockMemDb(worker), ap.call(t, p, "mock-in"))
				p.CloseTestWindow(1)
				ap.call(t, p, "mock-out")
				check(t, worker.GetConsumedMocks())
				got, err := p.GetConsumedMocks(ctx)
				require.NoError(t, err)
				require.Empty(t, got, "the worker's answers were the run's")
			})
		})
	}
}
