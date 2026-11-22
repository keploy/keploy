package captureloss

import (
	"reflect"
	"strings"
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
		{Kind: models.CaptureLossSTARTTLS, Count: 1, What: "to db:5432", HostsListed: true},
		{Kind: models.CaptureLossTLSAbort, Count: 2, What: "to 10.0.0.2:443", HostsListed: true},
	}
	if !reflect.DeepEqual(got, want) {
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
	want := models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 5, What: "the latest", HostsListed: true}}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want[0]) {
		t.Fatalf("Snapshot() = %+v, want %+v", got, want)
	}
}

// Each loss lists the hosts its What names, with their tags (the latest
// loss's, as its What is), and says it lists them: keploy's CLI logs each by
// its shape and tag, and reads nothing else in What as a host.
func TestALossListsTheHostsItsWhatNames(t *testing.T) {
	var c Counter
	keys := models.NewLogKeySession()
	first := keys.LossText().Say("to ").Host("orders-svc:443")
	latest := keys.LossText().Say("to ").Host("10-0-0-1.default.pod.cluster.local:443")
	c.Add(models.CaptureLossTLSAbort, first.String(), first.Hosts()...)
	c.Add(models.CaptureLossTLSAbort, latest.String(), latest.Hosts()...)
	c.Add(models.CaptureLossSTARTTLS, "a TLS upgrade the parser asked for")
	got := c.Snapshot()
	want := models.CaptureLosses{
		{Kind: models.CaptureLossSTARTTLS, Count: 1, What: "a TLS upgrade the parser asked for", HostsListed: true},
		{Kind: models.CaptureLossTLSAbort, Count: 2, What: "to 10-0-0-1.default.pod.cluster.local:443",
			Hosts: models.LossHosts{{Name: "10-0-0-1.default.pod.cluster.local:443", Tag: keys.HostTag("10-0-0-1.default.pod.cluster.local"), At: len("to ")}}, HostsListed: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() = %+v, want %+v", got, want)
	}
	if text := got.LogText(); strings.Contains(text, "pod.cluster.local") || !strings.Contains(text, keys.HostLogName("10-0-0-1.default.pod.cluster.local:443")) {
		t.Fatalf("LogText = %q: want the host by its shape and tag", text)
	}
}
