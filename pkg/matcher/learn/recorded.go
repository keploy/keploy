package learn

import (
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// recordingBudget, where a row sets it (0 or more), caps the bytes of
// decoded recordings the set keeps (Tracker.KeepRecording) within the set's
// budget (setBudget); noCap, no cap of its own, in keploy itself.
var recordingBudget int64 = noCap

// partRecording is the one part the set's tier of decoded recordings
// (setDerived.recs) keeps of a recording: the recording itself.
const partRecording part = 1

// KeepRecording is the recording key names, as decode reads it from the
// agent's on-disk store, kept decoded for the tracker's set: each call of
// the learn step reads every recording of the set made before it, and
// decoding each again on every call cost a call more than the rest of its
// reads of them. The set keeps it only in the room its budget has spare
// (setBudget): never past it, and the first let go when any other part of
// the set needs room, so the set's memory is what it was, and a recording
// the set has no room for is decoded at each read, as before. The set
// keeps it as decode gave it; each caller gets a copy of its own
// (models.Mock.ShallowCopy), as the store gave one per read before, whose
// fields of its own it may set, its response's too (HydrateResponse). A
// key names one recording of one store as decode reads it: a store that
// reads it otherwise from now on (DiskMocks.SetOnLoad) names it anew. The
// tracker's next set (NewSet) keeps none of them.
func (t *Tracker) KeepRecording(key string, size int64, decode func() (*models.Mock, error)) (*models.Mock, error) {
	if t == nil {
		return decode()
	}
	t.mu.Lock()
	s := t.derived
	t.mu.Unlock()
	return s.keepRecording(key, size, decode)
}

// keepRecording is KeepRecording on s: size is about the bytes the decoded
// recording holds.
func (s *setDerived) keepRecording(key string, size int64, decode func() (*models.Mock, error)) (*models.Mock, error) {
	if s == nil {
		return decode()
	}
	s.mu.Lock()
	if sl := s.recs.find(key, partRecording); sl != nil {
		m := sl.p
		s.mu.Unlock()
		return m.ShallowCopy(), nil
	}
	s.mu.Unlock()
	m, err := decode()
	if err != nil || m == nil {
		return m, err
	}
	s.mu.Lock()
	s.recDecodes++
	s.keepSpare(key, size, m)
	s.mu.Unlock()
	return m.ShallowCopy(), nil
}

// keepSpare keeps m under key in the recordings' tier (recs), within the
// tier's cap and the room the set's budget has spare: it lets go of the
// recordings it read least recently to make room, never another tier's
// part, and keeps none where the other tiers leave no room for it. s.mu is
// held.
func (s *setDerived) keepSpare(key string, size int64, m *models.Mock) {
	t := &s.recs
	if t.by[key] != nil {
		return
	}
	size += slotBytes + int64(len(key))
	if size > t.cap || s.budget.used-t.used+size > s.budget.limit {
		return
	}
	for t.used+size > t.cap || s.budget.used+size > s.budget.limit {
		if !t.letGoLast() {
			return
		}
	}
	sl := &slot[*models.Mock]{id: strings.Clone(key), p: m, has: partRecording, size: size}
	sl.at = t.order.PushFront(sl)
	t.by[sl.id] = sl
	t.used += size
	s.budget.used += size
}

// RecordingDecodes is how many recordings the tracker's set has decoded
// for its readers (KeepRecording): each kept one once, and each it had no
// room for at each read, for the rows.
func (t *Tracker) RecordingDecodes() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	s := t.derived
	t.mu.Unlock()
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recDecodes
}
