package http

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// gapDb is a store under exact serving and today's rules (no background
// cursor, as a set recorded without proven routing is served): its view
// and the recordings it stages, spent ones included.
type gapDb struct {
	mockMemDb
	view   integrations.SequenceView
	staged []*models.Mock
}

func (d *gapDb) SequenceView() (integrations.SequenceView, bool) { return d.view, true }
func (d *gapDb) StagedMocks() []*models.Mock                     { return d.staged }

// A beforeAll's page reads a call once in the gap before the first test
// (its recording is the pool slice's), and the tests after it record the
// same call in their own windows. In the replay the page reads it twice in
// the gap. Between tests the walk holds only what the gap's view can serve
// (SequenceView.Gap): the second read is the gap's last recording again,
// in every pass (cursorPick, the strict pass, the variable pass), never a
// miss naming a later test's own recording, which the gap cannot use.
// Without Gap (a view that says nothing) the walk is every staged
// recording's, as before: the second read misses on the last test's.
func TestBetweenTestsTheGapsLastRecordingAnswersAgain(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Unix(1_700_000_000, 0)
	setup := statefulAt("setup", base)
	t2 := statefulAt("t2-own", base.Add(2*time.Second))
	t4 := statefulAt("t4-own", base.Add(4*time.Second))
	notOwn := func(*models.Mock) bool { return false }
	every := func(*models.Mock) bool { return true }
	inGap := func(m *models.Mock) bool { return m != nil && m.Name == "setup" }
	h := newHTTP()
	for _, p := range []struct {
		name string
		pick func(db *gapDb, live []*models.Mock) (*models.Mock, func(), *answeredSequence)
	}{
		{"cursorPick", func(db *gapDb, live []*models.Mock) (*models.Mock, func(), *answeredSequence) {
			return h.cursorPick(live[0], live, notOwn, db, true)
		}},
		{"the strict pass", func(db *gapDb, live []*models.Mock) (*models.Mock, func(), *answeredSequence) {
			return h.strictCursorPick(live, notOwn, db, true, every)
		}},
		{"the variable pass", func(db *gapDb, live []*models.Mock) (*models.Mock, func(), *answeredSequence) {
			hits := make([]*varHit, 0, len(live))
			for _, m := range live {
				hits = append(hits, &varHit{mock: m})
			}
			hit, commit, answered := h.pickHit(hits, notOwn, db, true, every)
			if hit == nil {
				return nil, commit, answered
			}
			return hit.mock, commit, answered
		}},
	} {
		gap := func(cursor *mockMemDb, member func(*models.Mock) bool) *gapDb {
			return &gapDb{staged: []*models.Mock{setup, t2, t4}, view: integrations.SequenceView{Cursor: cursor, Gap: member}}
		}
		cursor := &mockMemDb{}
		for i := 1; i <= 2; i++ {
			m, commit, answered := p.pick(gap(cursor, inGap), []*models.Mock{setup})
			if answered != nil {
				t.Fatalf("%s: read %d in the gap missed: %s", p.name, i, answered.why())
			}
			if m != setup {
				t.Fatalf("%s: read %d in the gap = %v; want setup, the gap's own (then again)", p.name, i, m)
			}
			if commit != nil {
				commit()
			}
		}
		// A view that says nothing of the gap walks every staged recording,
		// as before: the second read misses on t4-own.
		cursor = &mockMemDb{}
		if m, commit, answered := p.pick(gap(cursor, nil), []*models.Mock{setup}); answered != nil || m != setup {
			t.Fatalf("%s, no Gap: the first read = %v (%v); want setup", p.name, m, answered)
		} else if commit != nil {
			commit()
		}
		if _, _, answered := p.pick(gap(cursor, nil), []*models.Mock{setup}); answered == nil || !strings.Contains(answered.why(), "t4-own") {
			t.Fatalf("%s, no Gap: the second read = %v; want the miss naming t4-own", p.name, answered)
		}
	}
}
