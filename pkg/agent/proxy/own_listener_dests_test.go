package proxy

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// The set's destinations of the calls passed through as the app's own: each
// once, in one spelling, sorted, never one that is no ip:port, at most
// models.MaxOwnListenerCalls, and none after the next set's Mock.
func TestTheSetsOwnListenerCallsAreBoundedAndEndWithTheSet(t *testing.T) {
	p := New(zap.NewNop(), nil, &config.Config{})
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	if got := p.OwnListenerCalls(); got != nil {
		t.Fatalf("a fresh agent reports %v, want none", got)
	}
	d := &p.ownListenerDests
	d.add("172.18.0.3:6100")
	d.add("[::ffff:172.18.0.3]:6100") // the same destination, IPv4-mapped
	d.add("127.0.0.1:6100")
	d.add("localhost:6100") // a name is not where a connection went
	d.add("no-port")
	if got, want := p.OwnListenerCalls(), []string{"127.0.0.1:6100", "172.18.0.3:6100"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reported %v, want %v", got, want)
	}
	for i := 0; i < models.MaxOwnListenerCalls+5; i++ {
		d.add(fmt.Sprintf("10.0.%d.%d:80", i/250, i%250))
	}
	if got := len(p.OwnListenerCalls()); got != models.MaxOwnListenerCalls {
		t.Fatalf("reported %d destinations, want the bound %d", got, models.MaxOwnListenerCalls)
	}
	// One already reported stays when the set holds the bound.
	d.add("172.18.0.3:6100")
	if got := p.OwnListenerCalls(); got[len(got)-1] != "172.18.0.3:6100" {
		t.Fatalf("a destination reported before the bound was lost: %v", got)
	}
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if got := p.OwnListenerCalls(); got != nil {
		t.Fatalf("the next set's agent reports the last set's %d destinations", len(got))
	}
}
