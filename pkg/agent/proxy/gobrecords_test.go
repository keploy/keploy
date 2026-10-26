package proxy

import (
	"bytes"
	"encoding/gob"
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// TestAKeptDecoderRefusesWhatItDoesNotReadWhole: a record a kept decoder
// reads only part of fails the read, and the decoder is not kept.
func TestAKeptDecoderRefusesWhatItDoesNotReadWhole(t *testing.T) {
	d := newStoreWith(t, residentBudgetBytes, perTestMockAt("a", 1))
	defer d.Close()
	e := d.byName["a"]
	rec, err := d.bytesAt(e.off, e.length)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRecord[models.Mock](d, e.off, rec); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	built := d.DecodersBuilt()
	padded := append(append([]byte(nil), rec...), rec...)
	if _, err := decodeRecord[models.Mock](d, e.off, padded); !errors.Is(err, errGobRecord) {
		t.Fatalf("a record followed by more bytes read with %v, want errGobRecord", err)
	}
	if _, err := decodeRecord[models.Mock](d, e.off, rec); err != nil {
		t.Fatal(err)
	}
	if got := d.DecodersBuilt(); got != built+1 {
		t.Fatalf("after a refused read, %d decoders built, want the refused one dropped and one more (%d)", got, built+1)
	}
}

// TestAStoreKeepsAtMostMaxKeptDecodersIdle: a burst of readers builds a
// decoder each, and the store keeps no more than maxKeptDecoders of them.
func TestAStoreKeepsAtMostMaxKeptDecodersIdle(t *testing.T) {
	d := newStoreWith(t, residentBudgetBytes)
	defer d.Close()
	var ks []*keptDecoder
	for i := 0; i < maxKeptDecoders+8; i++ {
		ks = append(ks, d.records.get())
	}
	for _, k := range ks {
		d.records.put(k)
	}
	if got := len(d.records.free); got != maxKeptDecoders {
		t.Fatalf("%d idle decoders kept, want %d", got, maxKeptDecoders)
	}
}

// TestAStreamThatLostALessonGoesOnInStreamsOfTheirOwn: a lesson the store
// fails to keep leaves the encoder having sent definitions no lesson holds,
// so every record after it is a gob stream of its own, which a new decoder
// reads alone.
func TestAStreamThatLostALessonGoesOnInStreamsOfTheirOwn(t *testing.T) {
	g := newGobRecords()
	failing := func([]byte) (int64, int, error) { return 0, 0, errors.New("disk full") }
	if _, err := g.encodeLocked(perTestMockAt("first", 1), false, 0, failing); err == nil {
		t.Fatal("a lesson the store failed to keep was not an error")
	}
	var kept [][]byte
	keeping := func(b []byte) (int64, int, error) { kept = append(kept, b); return int64(len(kept)), len(b), nil }
	b, err := g.encodeLocked(perTestMockAt("second", 2), false, 0, keeping)
	if err != nil {
		t.Fatal(err)
	}
	if g.freshFrom != 0 {
		t.Fatalf("records from %d are streams of their own, want from 0", g.freshFrom)
	}
	var m models.Mock
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&m); err != nil || m.Name != "second" {
		t.Fatalf("the record after the lost lesson does not read alone: %v (%q)", err, m.Name)
	}
}
