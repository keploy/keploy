package integrations

import (
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// unmadeStore serves exactly and says write is unmade before any read; it
// holds consumed as the open test's answered recordings and, when before is
// set, every recording made before an instant.
type unmadeStore struct {
	exactStore
	write    *models.Mock
	consumed []*models.Mock
	before   []*models.Mock
	ranges   bool
}

func (s unmadeStore) UnmadeWriteBefore(*models.Mock, func(*models.Mock) bool) (*models.Mock, bool) {
	return s.write, s.write != nil
}

func (s unmadeStore) ConsumedTestMocks() []*models.Mock { return s.consumed }

// noWriteStore says a write is unmade and names none.
type noWriteStore struct{ unmadeStore }

func (noWriteStore) UnmadeWriteBefore(*models.Mock, func(*models.Mock) bool) (*models.Mock, bool) {
	return nil, true
}

// rangingStore is unmadeStore that also reads the recordings made before an
// instant (RecordedBeforeReader).
type rangingStore struct{ unmadeStore }

func (s rangingStore) RangeRecordedBefore(t time.Time, visit func(*models.Mock) bool) error {
	for _, mk := range s.before {
		if !mk.Spec.ReqTimestampMock.After(t) && !visit(mk) {
			return nil
		}
	}
	return nil
}

func unmadeRec(name string, ms int, op string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{
		Metadata:         map[string]string{"op": op},
		ReqTimestampMock: time.Unix(0, 0).Add(time.Duration(ms) * time.Millisecond),
	}}
}

func isWriteRec(mk *models.Mock) bool { return mk.Spec.Metadata["op"] == "write" }
func sameRead(mk *models.Mock) bool   { return mk.Spec.Metadata["op"] == "read" }

// The rule decides only under exact serving, for a read, over a store that
// says a write is unmade; a store that says so but cannot read what was
// recorded before the write fails the call, never answering with the
// recording chosen.
func TestReadBeforeUnmadeWrite_WhatTheStoreCannotTellKeepsTheChoiceOrFails(t *testing.T) {
	write, chosen := unmadeRec("w", 100, "write"), unmadeRec("r", 110, "read")
	earlier := unmadeRec("r0", 10, "read")
	exact := unmadeStore{exactStore: exactStore{exact: true}, write: write}
	for _, tc := range []struct {
		name    string
		db      MockMemDb
		chosen  *models.Mock
		isWrite func(*models.Mock) bool
		same    func(*models.Mock) bool
		applies bool
		answer  string
		err     error
	}{
		{name: "exact serving off", db: unmadeStore{exactStore: exactStore{exact: false}, write: write}, chosen: chosen, isWrite: isWriteRec, same: sameRead},
		{name: "a store that cannot tell an unmade write", db: exactStore{exact: true}, chosen: chosen, isWrite: isWriteRec, same: sameRead},
		{name: "no write is unmade", db: unmadeStore{exactStore: exactStore{exact: true}}, chosen: chosen, isWrite: isWriteRec, same: sameRead},
		{name: "a store that names no unmade write", db: noWriteStore{exact}, chosen: chosen, isWrite: isWriteRec, same: sameRead},
		{name: "a write chosen is no read", db: rangingStore{exact}, chosen: write, isWrite: isWriteRec, same: sameRead},
		{name: "no recording chosen", db: rangingStore{exact}, isWrite: isWriteRec, same: sameRead},
		{name: "no write classifier", db: rangingStore{exact}, chosen: chosen, same: sameRead},
		{name: "no comparison", db: rangingStore{exact}, chosen: chosen, isWrite: isWriteRec},
		{name: "a store that cannot read what was recorded before", db: unmadeStore{exactStore: exactStore{exact: true}, write: write, before: []*models.Mock{earlier}}, chosen: chosen, isWrite: isWriteRec, same: sameRead, applies: true, err: errNoEarlierRecordings},
		{name: "the test's own answered recording, read without the recording", db: unmadeStore{exactStore: exactStore{exact: true}, write: write, consumed: []*models.Mock{earlier}}, chosen: chosen, isWrite: isWriteRec, same: sameRead, applies: true, answer: "r0"},
		{name: "the recording's", db: rangingStore{unmadeStore{exactStore: exactStore{exact: true}, write: write, before: []*models.Mock{earlier}}}, chosen: chosen, isWrite: isWriteRec, same: sameRead, applies: true, answer: "r0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadBeforeUnmadeWrite(tc.db, tc.chosen, tc.isWrite, tc.same)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v; want %v", err, tc.err)
			}
			if got.Applies() != tc.applies {
				t.Fatalf("applies = %v; want %v", got.Applies(), tc.applies)
			}
			name := ""
			if got.Answer != nil {
				name = got.Answer.Name
			}
			if name != tc.answer {
				t.Fatalf("answer = %q; want %q", name, tc.answer)
			}
			if tc.answer != "" && got.Answer == earlier {
				t.Fatal("the answer is the store's recording itself; want a copy: a parser that serves it must not write the staged recording")
			}
			if tc.applies && tc.answer == "" && tc.err == nil && got.Note() == "" {
				t.Fatal("a miss with no note")
			}
			if tc.answer != "" && got.Note() != "" {
				t.Fatalf("an answered read notes a miss: %q", got.Note())
			}
		})
	}
}
