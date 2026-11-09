package learn

import (
	"reflect"
	"strings"
	"sync/atomic"

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
// the set needs room, so the set's memory is what it was. A recording
// whose least size has no room is decoded at each read, as before, and
// not measured; one whose least size has room but whose measured heap has
// none is measured once, and the set remembers its key (keepSpare), so
// each later read decodes it, as before, and measures it no more. The set
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

// keepRecording is KeepRecording on s: size is the least the caller knows
// the decoded recording holds (its record's bytes and more); the set counts
// what it measures the recording holds (recordingHeap) where that is more.
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
	room := s.recRoom(key, size)
	s.mu.Unlock()
	// Measured outside the set's lock, and only where the least size has
	// room and the set has not refused the recording before: one it has
	// refused is decoded at each read, as before, and not measured again.
	if room {
		size = max(size, recordingHeap(m))
		s.mu.Lock()
		s.keepSpare(key, size, m)
		s.mu.Unlock()
	}
	return m.ShallowCopy(), nil
}

// recRoom reports whether the recordings' tier may keep a recording of size
// bytes under key (keepSpare's first checks), at the least, and the set
// has not refused it before (refused): s.mu is held.
func (s *setDerived) recRoom(key string, size int64) bool {
	t := &s.recs
	size += slotBytes + int64(len(key))
	return t.by[key] == nil && s.refused.by[key] == nil && size <= t.cap && s.budget.used-t.used+size <= s.budget.limit
}

// recordingMeasures counts the recordings recordingHeap measured, for the
// rows: one atomic add beside a walk of the whole recording.
var recordingMeasures atomic.Int64

// recordingHeap is about the heap m holds, as the runtime allocates it
// (allocBytes): its struct, each string's bytes, each slice's array, each
// map's header and entries (mapSlotBytes), and what each pointer and
// interface holds, each pointer and map counted once; a func's closure is
// not counted. A map's entries are each counted at the most an entry costs
// at any fill of its table (mapSlotBytes), up to about twice and a third
// what a map of that length holds: a size never under the heap. A recording's record on disk says little of it: an entry of a
// header map is a few bytes encoded and about a hundred in its map, so a
// recording of thousands of headers holds about eleven times its record,
// and a size taken from the record kept that much more than the set's
// budget counted.
func recordingHeap(m *models.Mock) int64 {
	recordingMeasures.Add(1)
	seen := map[uintptr]bool{}
	var walk func(v reflect.Value) int64
	walk = func(v reflect.Value) int64 {
		var n int64
		switch v.Kind() {
		case reflect.String:
			n = allocBytes(v.Len(), false)
		case reflect.Pointer:
			if v.IsNil() || seen[v.Pointer()] {
				return 0
			}
			seen[v.Pointer()] = true
			n = allocBytes(v.Type().Elem().Size(), true) + walk(v.Elem())
		case reflect.Interface:
			if v.IsNil() {
				return 0
			}
			if e := v.Elem(); e.Kind() == reflect.Pointer {
				n = walk(e)
			} else {
				n = allocBytes(e.Type().Size(), true) + walk(e)
			}
		case reflect.Slice:
			if v.IsNil() {
				return 0
			}
			n = allocBytes(uintptr(v.Cap())*v.Type().Elem().Size(), true)
			for i := range v.Len() {
				n += walk(v.Index(i))
			}
		case reflect.Array:
			for i := range v.Len() {
				n += walk(v.Index(i))
			}
		case reflect.Struct:
			for i := range v.NumField() {
				n += walk(v.Field(i))
			}
		case reflect.Map:
			if v.IsNil() || seen[v.Pointer()] {
				return 0
			}
			seen[v.Pointer()] = true
			slot := v.Type().Key().Size() + v.Type().Elem().Size()
			if n = mapHeaderBytes + mapGroupBytes(slot, true); v.Len() > 8 {
				n = mapHeaderBytes + int64(v.Len())*mapSlotBytes(slot, true)
			}
			for it := v.MapRange(); it.Next(); {
				n += walk(it.Key()) + walk(it.Value())
			}
		}
		return n
	}
	if m == nil {
		return 0
	}
	return walk(reflect.ValueOf(m))
}

// keepSpare keeps m under key in the recordings' tier (recs), within the
// tier's cap and the room the set's budget has spare: it lets go of the
// recordings it read least recently to make room, never another tier's
// part, and keeps none where the other tiers leave no room for it. A key it
// keeps none of it remembers (refused), on the set's budget, so recRoom
// refuses it from then on, before a reader measures the recording again.
// s.mu is held.
func (s *setDerived) keepSpare(key string, size int64, m *models.Mock) {
	t := &s.recs
	if t.by[key] != nil {
		return
	}
	size += slotBytes + int64(len(key))
	if size > t.cap || s.budget.used-t.used+size > s.budget.limit {
		s.refused.keep(key, partRecording, 0, func(*struct{}) {})
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
