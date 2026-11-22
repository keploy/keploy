//go:build linux

package proxy

import (
	"errors"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// A close of a connection of no worker's (instance 0) is, while a run of one
// test process holds the session (PairedBytesOneProcess on), that process's
// open test's: an owned loss stamped with worker 0's test epoch as it is
// counted, and in no count, so the agent charges it to the test begun at that
// epoch alone and a close counted in the gap between the process's tests to
// no test. Turning it on moves the epoch, so a close counted before the first
// begin is stamped with one no test began at. Before it is turned on, and once
// it is turned off again, such a close is the run's count, as keploy test's
// test cases read it. For a close keploy cannot account for and for bytes it
// saw lost.
func TestACloseOfTheOneProcessIsItsOpenTestsWhileItHoldsTheSession(t *testing.T) {
	for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
		for _, state := range []string{"never told", "on, in a test", "on, before the first begin", "on, then off"} {
			t.Run(string(kind)+"/"+state, func(t *testing.T) {
				p, app, tracked := untoldPair(t)
				pc := pairedConnOf(t, p, 1)
				if pc.instance != 0 {
					t.Fatalf("premise: the connection is of no worker's, got instance %d", pc.instance)
				}
				use(t, app, tracked)
				var claimed, begun uint64
				switch state {
				case "on, in a test":
					p.PairedBytesOneProcess(true)
					claimed = p.paired.epochs[0]
					begun = p.PairedBytesBegun(0)
				case "on, before the first begin":
					p.PairedBytesOneProcess(true)
					claimed = p.paired.epochs[0]
				case "on, then off":
					p.PairedBytesOneProcess(true)
					p.PairedBytesBegun(0)
					p.PairedBytesOneProcess(false)
				}
				if kind == models.CaptureLossAppBytesUntold {
					p.closeUnknown(pc, errors.New("netlink: no buffer space"))
				} else {
					p.pairLost(pc, 1, 5, unreadLost)
				}
				owned := p.TakeOwnedLosses()
				counted := lossesOf(p, kind)
				if state == "never told" || state == "on, then off" {
					if len(owned) != 0 || counted != 1 {
						t.Fatalf("owned %+v, %d counted; want none owned and one counted: no run of one process holds the session", owned, counted)
					}
					return
				}
				if claimed == 0 {
					t.Fatalf("turning a run of one process on did not move worker 0's epoch")
				}
				want := agent.OwnedLoss{Worker: 0, Open: true, Epoch: claimed, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: "from " + pc.app.String()}}
				if begun != 0 {
					if begun == claimed {
						t.Fatalf("premise: the begin moves the epoch past the claim's, both %d", begun)
					}
					want.Epoch = begun
				}
				if len(owned) != 1 || !reflect.DeepEqual(owned[0], want) {
					t.Fatalf("owned %+v, want %+v", owned, want)
				}
				if counted != 0 {
					t.Errorf("%d counted, want none: a count charges the test open when the agent reads it, a gap's close the next test", counted)
				}
				if state == "on, before the first begin" {
					if next := p.PairedBytesBegun(0); next == want.Epoch {
						t.Errorf("the first test begins at epoch %d, the one a close counted before it carries", next)
					}
				}
			})
		}
	}
}
