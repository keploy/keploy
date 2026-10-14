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
