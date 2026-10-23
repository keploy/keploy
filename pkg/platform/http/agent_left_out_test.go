package http_test

// What the agent leaves out of the pool it serves (its Redis mocks, in a build
// that skips Redis) it says in its /storemocks answer (models.StoreMocksResp),
// and keploy test takes it from there, never from a judging of its own: the
// two may be different builds (keploy test on macOS or Windows drives an
// agent in a Linux container). These run the real routes against the real
// client.

import (
	"bytes"
	"context"
	"encoding/gob"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.uber.org/zap"
)

// leftOutSvc is an agent service that left names out of the set it stored,
// and skips Redis, or not.
type leftOutSvc struct {
	stubSvc
	skips bool
	names []string
}

// StoreMocksStream stores as the stub does, and answers as the agent does:
// what it left out, written into the store's answer
// (models.WithStoreMocksAnswer).
func (s *leftOutSvc) StoreMocksStream(ctx context.Context, h models.MockStreamHeader, dec *gob.Decoder) error {
	if err := s.stubSvc.StoreMocksStream(ctx, h, dec); err != nil {
		return err
	}
	if into := models.StoreMocksAnswerOf(ctx); into != nil {
		into.SkipsRedis, into.LeftOut = s.skips, s.names
	}
	return nil
}

func serveLeftOut(t *testing.T, svc *leftOutSvc) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// stale is what storeAnswer's answer holds before the store: a store that
// writes nothing leaves it so.
var stale = models.StoreMocksResp{SkipsRedis: true, LeftOut: []string{"stale"}}

// storeAnswer stores f and u with c and returns the answer the store wrote
// where its context says (models.WithStoreMocksAnswer), stale when it wrote
// none.
func storeAnswer(ctx context.Context, c *httpclient.AgentClient, f, u []*models.Mock) (models.StoreMocksResp, error) {
	ans := models.StoreMocksResp{SkipsRedis: stale.SkipsRedis, LeftOut: append([]string(nil), stale.LeftOut...)}
	err := c.StoreMocks(models.WithStoreMocksAnswer(ctx, &ans), f, u)
	return ans, err
}

// isStale reports whether ans is still stale: the store wrote no answer.
func isStale(ans models.StoreMocksResp) bool {
	return ans.SkipsRedis == stale.SkipsRedis && reflect.DeepEqual(ans.LeftOut, stale.LeftOut)
}

func TestWhatTheAgentLeavesOutComesFromItsAnswerToTheStore(t *testing.T) {
	ctx := context.Background()
	f, u := fixtures()
	t.Run("an agent that skips Redis names what it left out", func(t *testing.T) {
		c := newClient(t, serveLeftOut(t, &leftOutSvc{skips: true, names: []string{"mock-2", "mock-10"}}).URL+"/agent")
		ans, err := storeAnswer(ctx, c, f, u)
		if err != nil {
			t.Fatal(err)
		}
		if !ans.SkipsRedis || !reflect.DeepEqual(ans.LeftOut, []string{"mock-2", "mock-10"}) {
			t.Fatalf("the store's answer is skipsRedis=%v %v, want true [mock-2 mock-10]", ans.SkipsRedis, ans.LeftOut)
		}
	})
	t.Run("an agent that records Redis leaves nothing out", func(t *testing.T) {
		c := newClient(t, serveLeftOut(t, &leftOutSvc{}).URL+"/agent")
		ans, err := storeAnswer(ctx, c, f, u)
		if err != nil {
			t.Fatal(err)
		}
		if ans.SkipsRedis || len(ans.LeftOut) != 0 {
			t.Fatalf("the store's answer is skipsRedis=%v %v from an agent that leaves nothing out", ans.SkipsRedis, ans.LeftOut)
		}
	})
	t.Run("an agent that predates the answer leaves nothing out, the legacy framing's too; a failed store answers nothing", func(t *testing.T) {
		mode := "new"
		mux := http.NewServeMux()
		mux.HandleFunc("/agent/storemocks", func(w http.ResponseWriter, r *http.Request) {
			var discard bytes.Buffer
			_, _ = discard.ReadFrom(r.Body)
			switch {
			case mode == "fail":
				w.WriteHeader(http.StatusInternalServerError)
				_ = gob.NewEncoder(w).Encode(models.AgentResp{ErrorMsg: "no session"})
			case mode == "legacy" && r.Header.Get("Content-Type") == models.StoreMocksStreamContentType:
				// An agent that predates streaming refuses the stream.
				w.WriteHeader(http.StatusBadRequest)
			case mode == "old" || mode == "legacy":
				// What every agent answered before it said what it left out.
				w.WriteHeader(http.StatusOK)
				_ = gob.NewEncoder(w).Encode(models.AgentResp{IsSuccess: true})
			default:
				w.WriteHeader(http.StatusOK)
				_ = gob.NewEncoder(w).Encode(models.StoreMocksResp{IsSuccess: true, SkipsRedis: true, LeftOut: []string{"mock-1"}})
			}
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		c := newClient(t, srv.URL+"/agent")
		if ans, err := storeAnswer(ctx, c, f, u); err != nil || !ans.SkipsRedis || len(ans.LeftOut) != 1 {
			t.Fatalf("precondition: the store answered %+v, %v", ans, err)
		}
		for _, m := range []string{"old", "legacy"} {
			mode = m
			ans, err := storeAnswer(ctx, c, f, u)
			if err != nil {
				t.Fatalf("%s agent: %v", m, err)
			}
			if ans.SkipsRedis || len(ans.LeftOut) != 0 {
				t.Fatalf("an %s agent's answer reads as skipsRedis=%v %v, want nothing left out", m, ans.SkipsRedis, ans.LeftOut)
			}
		}
		mode = "fail"
		if ans, err := storeAnswer(ctx, c, f, u); err == nil || !isStale(ans) {
			t.Fatalf("a failed store answered %+v, %v, want an error and no answer written", ans, err)
		}
		srv.Close()
		if ans, err := storeAnswer(ctx, c, f, u); err == nil || !isStale(ans) {
			t.Fatalf("a store the agent never answered answered %+v, %v, want an error and no answer written", ans, err)
		}
	})
}

// A keploy test that predates models.StoreMocksResp reads its AgentResp
// fields alone, and a failure still reads as one.
func TestAStoreAnswerReadsAsAnAgentResp(t *testing.T) {
	for _, sent := range []models.StoreMocksResp{
		{IsSuccess: true, SkipsRedis: true, LeftOut: []string{"mock-1"}},
		{ErrorMsg: "no session"},
	} {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(sent); err != nil {
			t.Fatal(err)
		}
		var got models.AgentResp
		if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
			t.Fatalf("an old client cannot read %+v: %v", sent, err)
		}
		if got.IsSuccess != sent.IsSuccess || got.ErrorMsg != sent.ErrorMsg {
			t.Fatalf("an old client read %+v as %+v", sent, got)
		}
	}
}
