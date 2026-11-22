//go:build linux

package proxy

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
)

// The recording's stop has begun once keploy mock record's client says so
// (NoteStopBegun) or keploy record's graceful-shutdown notice arrives, and a
// new recording or mocking session starts with it not begun.
func TestStopBegunIsSetByEitherNoticeAndClearedByTheNextSession(t *testing.T) {
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{}, cfg)
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	if p.StopBegun() {
		t.Fatal("a new proxy has its stop begun")
	}
	if err := p.SetGracefulShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.StopBegun() {
		t.Fatal("keploy record's graceful-shutdown notice did not begin the stop")
	}
	if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}
	if p.StopBegun() {
		t.Fatal("a new recording started with the last one's stop begun")
	}
	p.NoteStopBegun()
	if !p.StopBegun() {
		t.Fatal("keploy mock record's stop notice did not begin the stop")
	}
	if p.IsGracefulShutdown() {
		t.Fatal("keploy mock record's stop notice set the graceful-shutdown flag, whose effects belong after its drain")
	}
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if p.StopBegun() {
		t.Fatal("a new mocking session started with the last recording's stop begun")
	}
}
