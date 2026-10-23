package http_test

import (
	"context"
	"errors"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"net/http/httptest"
)

// diskFile is a models.PayloadSource: the mock file a replay's read left the
// mocks' payloads in, holding each recorded mock by name.
type diskFile struct {
	recorded map[string]func() *models.Mock
	mutate   models.PayloadMutator
	err      error
	reads    int
}

func (d *diskFile) LoadPayload(_ context.Context, at models.PayloadAt) (*models.Mock, error) {
	d.reads++
	if d.err != nil {
		return nil, d.err
	}
	return d.recorded[at.Name](), nil
}
func (d *diskFile) SetPayloadMutator(m models.PayloadMutator) { d.mutate = m }
func (d *diskFile) PayloadMutator() models.PayloadMutator     { return d.mutate }

func recordedHTTP(name, url string) func() *models.Mock {
	return func() *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: url},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: "ENC:secret"},
		}}
	}
}

// onDisk is the mocks of file, each with its payload left in it, as the
// replay's read hands them over.
func onDisk(file *diskFile, names ...string) []*models.Mock {
	out := make([]*models.Mock, 0, len(names))
	for _, n := range names {
		m := file.recorded[n]()
		m.DeriveLifetime()
		m.LeavePayloadOnDisk(file, models.PayloadAt{Name: n})
		out = append(out, m)
	}
	return out
}

func agentServer(t *testing.T, svc *stubSvc) string {
	t.Helper()
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL + "/agent"
}

// A mock whose payload a replay's read left on disk reaches the agent whole:
// read back as it is streamed, and changed by the source's mutator (a
// decryption) on the way. The mock the caller holds keeps no payload.
func TestStoreMocks_SendsAMockWhosePayloadIsOnDiskWithIt(t *testing.T) {
	file := &diskFile{recorded: map[string]func() *models.Mock{
		"f1": recordedHTTP("f1", "http://orders/v1"),
		"u1": recordedHTTP("u1", "http://config/v1"),
	}}
	file.SetPayloadMutator(func(_ context.Context, m *models.Mock) error {
		m.Spec.HTTPResp.Body = "secret"
		return nil
	})
	f, u := onDisk(file, "f1"), onDisk(file, "u1")
	svc := &stubSvc{}
	client := newClient(t, agentServer(t, svc))
	if err := client.StoreMocks(context.Background(), f, u); err != nil {
		t.Fatalf("StoreMocks: %v", err)
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.filtered != 1 || svc.unfiltered != 1 || len(svc.mocks) != 2 {
		t.Fatalf("the agent got %d+%d mocks (%d decoded); want 1+1", svc.filtered, svc.unfiltered, len(svc.mocks))
	}
	for i, want := range []string{"http://orders/v1", "http://config/v1"} {
		got := svc.mocks[i]
		if got.Spec.HTTPReq == nil || got.Spec.HTTPReq.URL != want || got.Spec.HTTPResp == nil || got.Spec.HTTPResp.Body != "secret" {
			t.Fatalf("the agent got mock %s as %+v; want its payload read back and decrypted", got.Name, got.Spec)
		}
	}
	if f[0].Spec.HTTPReq != nil || !f[0].PayloadOnDisk() {
		t.Fatal("the caller's mock holds the payload it was sent with")
	}
	if file.reads != 2 {
		t.Fatalf("the payloads were read back %d times; want once each", file.reads)
	}
}

// A payload that cannot be read back fails the call with the reason, not with
// the agent's answer to a stream that ended early, and nothing is stored.
func TestStoreMocks_FailsAPayloadItCannotReadBack(t *testing.T) {
	file := &diskFile{recorded: map[string]func() *models.Mock{"f1": recordedHTTP("f1", "http://orders/v1")}}
	f := onDisk(file, "f1")
	file.err = models.ErrPayloadFileReplaced
	svc := &stubSvc{}
	client := newClient(t, agentServer(t, svc))
	err := client.StoreMocks(context.Background(), f, nil)
	if !errors.Is(err, models.ErrPayloadFileReplaced) {
		t.Fatalf("StoreMocks returned %v; want the payload's ErrPayloadFileReplaced", err)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.streamCalls != 0 {
		t.Fatal("the agent stored a stream that ended early")
	}
}
