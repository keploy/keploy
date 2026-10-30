package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/synhold"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// a destination-keyed (HTTP/2 authority) local dependency a started
// process reached first and the outside dependency reached later, only inside tests.
func TestAnH2PortAlsoReachedAsAnOutsideDependencyIsNoChild(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	shared := testnet.RefusedAddr(t, "tcp")
	s, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h2 := func(name string, sec int64, started bool) *models.Mock {
		m := perTestMockAt(name, sec)
		m.Kind = models.HTTP2
		m.Spec.HTTP2Req = &models.HTTP2Req{Scheme: "http", Authority: shared}
		m.Spec.Metadata = map[string]string{"destAddr": shared}
		if started {
			m.Spec.Metadata["startedByTests"] = "true"
		}
		return m
	}
	for _, m := range []*models.Mock{h2("started", 20, true), h2("outside-in-test", 25, false)} {
		if err := s.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	s.Finalize()
	decide := replayLocalPorts(t, true, s)
	if got := decide(shared); got != synhold.Accept {
		t.Fatalf("a stopped local h2 port reached by a started process and, only inside tests, as an outside dependency: %v, want accept", got)
	}
}

// a real proxy's Mock forgets the previous session's staged ports.
func TestAProxysMockForgetsTheStagedPorts(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	staged := testnet.RefusedAddr(t, "tcp")
	recorded.forget()
	t.Cleanup(recorded.forget)
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := New(zap.NewNop(), fakeDestInfo{}, config.New())
	p.nsswitchData = []byte("preseeded-by-test")
	m := perTestMockAt("staged", 5)
	m.Spec.Metadata = map[string]string{"destAddr": staged}
	if err := p.SetMocks(ctx, nil, []*models.Mock{m}); err != nil {
		t.Fatal(err)
	}
	port := uint32(mustPort(t, staged))
	if !RecordedPort(port) {
		t.Fatal("the staged port is not recorded")
	}
	if err := p.Mock(ctx, models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if RecordedPort(port) {
		t.Fatal("a Proxy.Mock kept the previous session's staged port")
	}
}
