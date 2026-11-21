package proxy

import (
	"slices"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// recordedWarms counts the sets whose recordings were handed to the
// parsers' warmers (setWarm): a warm whose number is no longer the last
// stops, since a set staged after it holds other recordings.
type recordedWarms struct {
	gen atomic.Uint64
}

// setWarm hands one staged set's recordings to the parsers' warmers
// (integrations.RecordingWarmer), each to its kind's, until every warmer
// said it can keep no more or a later set was staged.
type setWarm struct {
	ws      map[models.Kind]func(*models.Mock) bool
	full    map[models.Kind]bool
	gen     uint64
	of      *recordedWarms
	warmed  int
	stopped bool
}

// newSetWarm is the warm of the set just staged, numbered past every
// earlier one (which then stops), each registered warmer's warm of it
// begun, or nil where none reads anything: the proven switch off (the
// parsers' caches take nothing), or no warmer registered that reads the
// set.
func (p *Proxy) newSetWarm() *setWarm {
	if !provenswitch.On() {
		return nil
	}
	ws := map[models.Kind]func(*models.Mock) bool{}
	for k, begin := range integrations.RecordingWarmers() {
		if warm := begin(); warm != nil {
			ws[k] = warm
		}
	}
	if len(ws) == 0 {
		return nil
	}
	return &setWarm{ws: ws, full: map[models.Kind]bool{}, gen: p.recordedWarm.gen.Add(1), of: &p.recordedWarm}
}

// warm hands mk to its kind's warmer, unless that one is full; more is
// false once every warmer is full or a later set was staged (stopped).
func (w *setWarm) warm(mk *models.Mock) (more bool) {
	if w.of.gen.Load() != w.gen {
		w.stopped = true
		return false
	}
	if mk == nil {
		return true
	}
	if f, ok := w.ws[mk.Kind]; ok && !w.full[mk.Kind] {
		if f(mk) {
			w.warmed++
		} else {
			w.full[mk.Kind] = true
		}
	}
	return len(w.full) < len(w.ws)
}

// wrap is visit with each recording it is handed warmed first, while w
// warms: a warm that ended leaves visit's own walk as it is.
func (w *setWarm) wrap(visit func(*models.Mock) bool) func(*models.Mock) bool {
	warming := true
	return func(mk *models.Mock) bool {
		if warming {
			warming = w.warm(mk)
		}
		return visit(mk)
	}
}

// kinds is the kinds w's warmers read, in order: the only kinds the warm
// reads from the disk.
func (w *setWarm) kinds() []models.Kind {
	ks := make([]models.Kind, 0, len(w.ws))
	for k := range w.ws {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// log says at Debug what the warm read, and why it ended short where it did.
func (w *setWarm) log(logger *zap.Logger) {
	full := make([]string, 0, len(w.full))
	for k := range w.full {
		full = append(full, string(k))
	}
	logger.Debug("the parsers read the staged set's recordings ahead of its calls: the calls read the rest themselves, as with no warming",
		zap.Int("recordings_warmed", w.warmed), zap.Strings("kinds_full", full), zap.Bool("later_set_staged", w.stopped))
}

// WarmRecordedSet implements agent.RecordedSetWarmer: under the proven
// switch, the parsers' warmers read the staged set's recordings in the
// background, in recorded order across the trees and the disk
// (rangeRecordedInOrderFrom), so a replay's first calls find the parsers'
// reads of them made. Of the disk it reads, through ofKinds (the agent's
// on-disk store, DiskMocks.RangeRecordedOfKindsBefore; nil where the set is
// all in the trees), only the kinds a warmer reads: a recording of another
// kind is never decoded for the warm. In record's full round, where the
// learn step's staging walks the same recordings (warmLearnSet), it hands
// them to the warmers on that walk, and this reads nothing more. With no
// mock manager there is no set (errNoMockManager).
func (p *Proxy) WarmRecordedSet(ofKinds func(t time.Time, kinds []models.Kind, visit func(*models.Mock) bool) error) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	if m.learns() {
		return nil
	}
	w := p.newSetWarm()
	if w == nil {
		return nil
	}
	var disk func(time.Time, func(*models.Mock) bool) error
	if ofKinds != nil {
		kinds := w.kinds()
		disk = func(t time.Time, visit func(*models.Mock) bool) error { return ofKinds(t, kinds, visit) }
	}
	go func() {
		_ = m.rangeRecordedInOrderFrom(disk, lastRecorded, w.warm)
		w.log(p.logger)
	}()
	return nil
}

var _ agent.RecordedSetWarmer = (*Proxy)(nil)
