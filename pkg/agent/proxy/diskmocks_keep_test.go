package proxy

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// keptStore is a store of six per-test recordings named prefix0..prefix5,
// all on disk.
func keptStore(t *testing.T, prefix string) *DiskMocks {
	t.Helper()
	d, err := newDiskMocks(zap.NewNop(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for i := range 6 {
		m := perTestMockAt(fmt.Sprintf("%s%d", prefix, i), int64(100+i))
		m.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://svc/%s/%d", prefix, i)}
		if err := d.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	d.Finalize()
	return d
}

// recordedVisits are the recordings RangeRecordedBefore visits of d, all of
// them.
func recordedVisits(t *testing.T, d *DiskMocks) []*models.Mock {
	t.Helper()
	var out []*models.Mock
	if err := d.RangeRecordedBefore(time.Unix(1000, 0), func(m *models.Mock) bool {
		out = append(out, m)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// The learn step's reads of the set's recordings on disk
// (RangeRecordedBefore), kept by the learn tracker's set
// (learn.Tracker.KeepRecording), visit the same recordings in the same
// order, each equal to the one decoded at each read, whether the set
// decodes them now (cold), keeps them (warm) or has no room for them (a
// keeper that keeps none): what the learn step decides of them is the
// same. Warm, they are decoded once for the set, not once per call; with
// no room, or with no keeper, at each read. Each visit is handed a
// recording of its own.
func TestDiskRecordedBeforeKeptVisitsTheSameRecordingsDecodedOnce(t *testing.T) {
	d := keptStore(t, "m")
	plain := recordedVisits(t, d)
	if len(plain) != 6 || d.RecordedReads() != 6 {
		t.Fatalf("with no keeper: %d visits, %d decodes; want 6, 6", len(plain), d.RecordedReads())
	}
	tr := learn.NewTracker()
	tr.NewSet()
	d.SetRecordingKeeper(tr.KeepRecording)
	for _, c := range []struct {
		name  string
		reads int64
	}{{"cold", 12}, {"warm", 12}, {"warm again", 12}} {
		got := recordedVisits(t, d)
		if !reflect.DeepEqual(got, plain) {
			t.Errorf("%s: visited %v; want the recordings as decoded at each read %v", c.name, got, plain)
		}
		if d.RecordedReads() != c.reads {
			t.Errorf("%s: %d decodes in all; want %d", c.name, d.RecordedReads(), c.reads)
		}
		got[0].Name, got[0].Spec.HTTPReq = "edited", nil
	}
	d.SetRecordingKeeper(nil)
	if got := recordedVisits(t, d); !reflect.DeepEqual(got, plain) || d.RecordedReads() != 18 {
		t.Errorf("no keeper: visited %v with %d decodes in all; want %v with 18", got, d.RecordedReads(), plain)
	}
	d.SetRecordingKeeper(func(_ string, _ int64, decode func() (*models.Mock, error)) (*models.Mock, error) { return decode() })
	if got := recordedVisits(t, d); !reflect.DeepEqual(got, plain) || d.RecordedReads() != 24 {
		t.Errorf("no room: visited %v with %d decodes in all; want %v with 24", got, d.RecordedReads(), plain)
	}
}

// A store that reads its records otherwise from now on (SetOnLoad: what a
// set's first cycle learned, for its retry cycles) visits them as it reads
// them now, not as its keeper kept them; two stores' records, at the same
// offsets, are each their own store's; and the size a store hands its
// keeper is more than the recording's record, which the decoded recording
// holds and more.
func TestDiskRecordedBeforeKeptReadsAsTheStoreReadsNow(t *testing.T) {
	d := keptStore(t, "m")
	tr := learn.NewTracker()
	tr.NewSet()
	var sizes []int64
	d.SetRecordingKeeper(func(key string, size int64, decode func() (*models.Mock, error)) (*models.Mock, error) {
		sizes = append(sizes, size)
		return tr.KeepRecording(key, size, decode)
	})
	first := recordedVisits(t, d)
	for i, m := range first {
		if sizes[i] <= m.StoredSize() {
			t.Errorf("%s: handed its keeper as %d bytes; want more than its record's %d", m.Name, sizes[i], m.StoredSize())
		}
	}
	d.SetOnLoad(func(m *models.Mock) { m.Spec.Metadata = map[string]string{"learned": "yes"} })
	for _, m := range recordedVisits(t, d) {
		if m.Spec.Metadata["learned"] != "yes" {
			t.Errorf("%s: visited as kept before the store read it otherwise; want it as read now", m.Name)
		}
	}
	if d.RecordedReads() != 12 {
		t.Errorf("%d decodes in all; want 12, each record read again once the store reads it otherwise", d.RecordedReads())
	}
	other := keptStore(t, "n")
	other.SetRecordingKeeper(tr.KeepRecording)
	for i, m := range recordedVisits(t, other) {
		if want := fmt.Sprintf("n%d", i); m.Name != want {
			t.Errorf("another store's record %d visited as %s; want %s", i, m.Name, want)
		}
	}
	if other.RecordedReads() != 6 {
		t.Errorf("another store decoded %d of its six records; want 6", other.RecordedReads())
	}
}
