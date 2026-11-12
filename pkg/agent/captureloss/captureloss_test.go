package captureloss

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestCountsByKindWithTheLatestExample(t *testing.T) {
	var c Counter
	if got := c.Snapshot(); len(got) != 0 {
		t.Fatalf("a fresh counter: %+v", got)
	}
	c.Add(models.CaptureLossTLSAbort, "to 10.0.0.1:443")
	c.Add(models.CaptureLossTLSAbort, "to 10.0.0.2:443")
	c.Add(models.CaptureLossSTARTTLS, "to db:5432")
	got := c.Snapshot()
	want := models.CaptureLosses{
		{Kind: models.CaptureLossSTARTTLS, Count: 1, What: "to db:5432"},
		{Kind: models.CaptureLossTLSAbort, Count: 2, What: "to 10.0.0.2:443"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Snapshot() = %+v, want %+v", got, want)
	}
}

// AddN counts a read's n losses at once, as n Adds would, and zero counts
// nothing: no kind appears for it.
func TestAddNCountsSeveralAtOnce(t *testing.T) {
	var c Counter
	c.AddN(models.CaptureLossListenerDropped, 0, "none")
	if got := c.Snapshot(); len(got) != 0 {
		t.Fatalf("zero counted: %+v", got)
	}
	c.AddN(models.CaptureLossListenerDropped, 3, "")
	c.AddN(models.CaptureLossListenerDropped, 2, "the latest")
	got := c.Snapshot()
	want := models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 5, What: "the latest"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Snapshot() = %+v, want %+v", got, want)
	}
}
