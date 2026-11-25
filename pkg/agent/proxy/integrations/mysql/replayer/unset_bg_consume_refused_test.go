package replayer

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type bgAwareMockDb struct {
	*fakeMockDb
	exactServing bool
	s12BG        bool
	bgNames      map[string]bool
	refuseName   string
}

func (b *bgAwareMockDb) ExactServing() bool { return b.exactServing }

func (b *bgAwareMockDb) BackgroundCursor() (integrations.MockCursor, bool) {
	if !b.s12BG {
		return nil, false
	}
	return nopCursor{}, true
}

func (b *bgAwareMockDb) InBackground(name string) bool { return b.bgNames[name] }

func (b *bgAwareMockDb) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	if old != nil && old.Name == b.refuseName {
		return false
	}
	return b.fakeMockDb.UpdateUnFilteredMock(old, new)
}

func (b *bgAwareMockDb) MarkMockAsUsed(m models.Mock) bool {
	if m.Name == b.refuseName {
		return false
	}
	return b.fakeMockDb.MarkMockAsUsed(m)
}

type nopCursor struct{}

func (nopCursor) MockCursorIndex(string, int) int    { return 0 }
func (nopCursor) AdvanceMockCursor(string, int, int) {}

// TestMatchCommand_UnsetBackgroundArmedRefusedOwnMockDoesNotRetry proves B2:
// when KEPLOY_PROVEN is unset (ExactServing == false) and S12Background is
// active, integrations.ConsumeRefused MUST be false for own mocks (not in
// InBackground), so MySQL's matchCommand keeps main's behavior and returns
// errConsumeRefused instead of retrying past a refused own mock and serving
// another copy. Background names (InBackground == true) and exact serving
// (KEPLOY_PROVEN=1) still treat refused consumes as retriable refusals.
func TestMatchCommand_UnsetBackgroundArmedRefusedOwnMockDoesNotRetry(t *testing.T) {
	logger := zap.NewNop()
	const sql = "SELECT id FROM accounts WHERE slug = 'acme'"
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	t.Run("unset_own_refusal_returns_error_like_main", func(t *testing.T) {
		m1 := readbackMock("mock-own-1", sql, "row=1", base)
		m2 := readbackMock("mock-own-2", sql, "row=2", base.Add(time.Second))
		db := &bgAwareMockDb{
			fakeMockDb:   &fakeMockDb{session: []*models.Mock{m1, m2}},
			exactServing: false,
			s12BG:        true,
			bgNames:      map[string]bool{"mock-startup-bg": true},
			refuseName:   "mock-own-1",
		}
		if integrations.ConsumeRefused(db, "mock-own-1") {
			t.Fatal("ConsumeRefused must be false for an own mock when ExactServing is false")
		}
		if !integrations.ConsumeRefused(db, "mock-startup-bg") {
			t.Fatal("ConsumeRefused must remain true for an InBackground mock when S12Background is active")
		}
		resp, ok, _, err := matchCommand(context.Background(), logger, comQueryReq(sql), db, newDecodeCtx(), nil, nil)
		if ok || resp != nil {
			t.Fatalf("refused own mock in unset mode must NOT retry to serve another copy; got ok=%v resp=%v err=%v", ok, resp, err)
		}
		if !errors.Is(err, errConsumeRefused) {
			t.Fatalf("expected errConsumeRefused (main behavior), got %v", err)
		}
	})

	t.Run("unset_background_refusal_retries_to_next_copy", func(t *testing.T) {
		m1 := readbackMock("mock-bg-1", sql, "bg=1", base)
		m2 := readbackMock("mock-bg-2", sql, "bg=2", base.Add(time.Second))
		db := &bgAwareMockDb{
			fakeMockDb:   &fakeMockDb{session: []*models.Mock{m1, m2}},
			exactServing: false,
			s12BG:        true,
			bgNames:      map[string]bool{"mock-bg-1": true, "mock-bg-2": true},
			refuseName:   "mock-bg-1",
		}
		resp, ok, _, err := matchCommand(context.Background(), logger, comQueryReq(sql), db, newDecodeCtx(), nil, nil)
		if err != nil || !ok || resp == nil || resp.Payload != "bg=2" {
			t.Fatalf("refused InBackground mock must retry and serve bg=2; got ok=%v resp=%v err=%v", ok, resp, err)
		}
	})

	t.Run("exact_serving_own_refusal_retries_to_next_copy", func(t *testing.T) {
		m1 := readbackMock("mock-own-1", sql, "row=1", base)
		m2 := readbackMock("mock-own-2", sql, "row=2", base.Add(time.Second))
		db := &bgAwareMockDb{
			fakeMockDb:   &fakeMockDb{session: []*models.Mock{m1, m2}},
			exactServing: true,
			s12BG:        true,
			bgNames:      map[string]bool{"mock-startup-bg": true},
			refuseName:   "mock-own-1",
		}
		resp, ok, _, err := matchCommand(context.Background(), logger, comQueryReq(sql), db, newDecodeCtx(), nil, nil)
		if err != nil || !ok || resp == nil || resp.Payload != "row=2" {
			t.Fatalf("under ExactServing (KEPLOY_PROVEN=1) refused own mock must retry to row=2; got ok=%v resp=%v err=%v", ok, resp, err)
		}
	})
}
