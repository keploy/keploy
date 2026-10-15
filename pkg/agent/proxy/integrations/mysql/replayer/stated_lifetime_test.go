package replayer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	agentSvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// windowedProxy is the part of the proxy keploy test's staging reaches: it
// forwards the agent's stagings to a real mock manager, as proxy.Proxy does.
type windowedProxy struct {
	coreAgent.Proxy
	mm *proxy.MockManager
}

func (p *windowedProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, start, end time.Time) error {
	p.mm.SetMocksWithWindow(f, u, start, end)
	return nil
}

func (p *windowedProxy) FirstTestWindowStart() time.Time { return p.mm.FirstTestWindowStart() }
func (p *windowedProxy) GetPersistentConsumed() map[string]models.MockState {
	return p.mm.GetPersistentConsumed()
}

// A statement test A prepared on a pooled connection is executed in test B,
// as a statement cache does: B is served its own EXECUTE, and test C, which
// never recorded one and runs before B, misses it rather than being answered
// with B's. That is
// a recording whose recorder stated each mock's lifetime (the PREPARE
// connection, the EXECUTE per-test). A recording made before, whose EXECUTE
// the lax fallback serves to every test, is replayed as it always was.
func TestStatedExecuteIsServedOnlyToItsOwnTest(t *testing.T) {
	const query = "SELECT v FROM kv WHERE id = ?"
	windows := [3][2]time.Time{}
	for i := range windows {
		s := diffBase.Add(time.Duration(i) * 10 * time.Millisecond)
		windows[i] = [2]time.Time{s, s.Add(4 * time.Millisecond)}
	}
	for _, stated := range []bool{true, false} {
		name := "recording that states lifetimes"
		if !stated {
			name = "recording made before"
		}
		t.Run(name, func(t *testing.T) {
			prep := diffPrepareMock("prep-in-A", "c1", query, 1, windows[0][0].Add(time.Millisecond))
			exec := diffMock("exec-in-B", "c1", []mysql.Request{executeReq(1, "k1")}, windows[1][0].Add(time.Millisecond), models.LifetimePerTest, "mocks")
			for _, m := range []*models.Mock{prep, exec} {
				// As read from disk: the lifetime is derived again on load.
				m.TestModeInfo = models.TestModeInfo{}
			}
			if stated {
				prep.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimeConnection
				exec.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
			}
			for _, m := range []*models.Mock{prep, exec} {
				m.DeriveLifetime()
			}
			// The CLI hands the agent the per-test pool and the reusable one.
			var perTest, reusable []*models.Mock
			for _, m := range []*models.Mock{prep, exec} {
				if m.TestModeInfo.Lifetime == models.LifetimePerTest {
					perTest = append(perTest, m)
				} else {
					reusable = append(reusable, m)
				}
			}

			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			cfg := &config.Config{}
			cfg.Agent.Mode = models.MODE_TEST
			a := agentSvc.New(zap.NewNop(), nil, &windowedProxy{mm: mm}, nil, nil, cfg)
			ctx := context.Background()
			if err := a.StoreMocks(ctx, perTest, reusable); err != nil {
				t.Fatal(err)
			}
			stage := func(start, end time.Time) {
				t.Helper()
				// keploy test's filter for a test case: its recorded window,
				// strict as keploy.yaml has it by default.
				if err := a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: start, BeforeTime: end, StrictMockWindow: true, AgentOwnsConsumed: true}); err != nil {
					t.Fatal(err)
				}
			}
			stage(models.BaseTime, time.Now())

			dctx := &wire.DecodeContext{
				PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
				StmtIDToQuery:      map[uint32]string{7: query},
				NextStmtID:         100,
			}
			conn := context.WithValue(ctx, models.ClientConnectionIDKey, "c1")
			serve := func(req mysql.Request) string {
				t.Helper()
				resp, ok, _, err := replayer.MatchCommandForTest(conn, zap.NewNop(), req, mm, dctx, nil, false)
				if err != nil {
					t.Fatal(err)
				}
				if !ok || resp == nil {
					return ""
				}
				if resp.Payload != "" {
					return resp.Payload
				}
				return "served"
			}

			stage(windows[0][0], windows[0][1])
			if got := serve(prepareReq(query)); got == "" {
				t.Fatal("test A's PREPARE missed")
			}
			// Test C runs before B, so what it is served is no question of
			// what B consumed.
			stage(windows[2][0], windows[2][1])
			got := serve(executeReq(7, "k1"))
			if stated && got != "" {
				t.Fatalf("test C's EXECUTE, which it never recorded, was answered by %q; want a miss", got)
			}
			// A recording made before replays as it always did: its EXECUTE is
			// reusable under the lax fallback (KEPLOY_STRICT_MOCK_WINDOW not
			// enabling, the default), and test C is answered with it.
			if !stated && exec.TestModeInfo.Lifetime == models.LifetimeSession && !strings.Contains(got, "exec-in-B") {
				t.Fatalf("on a recording made before, test C's EXECUTE was answered by %q; want exec-in-B, as it always was", got)
			}
			stage(windows[1][0], windows[1][1])
			if got := serve(executeReq(7, "k1")); !strings.Contains(got, "exec-in-B") {
				t.Fatalf("test B's EXECUTE was answered by %q, want its own recording exec-in-B", got)
			}
		})
	}
}
