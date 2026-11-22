//go:build linux

package agent

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// drainingWire is a proxy with owned losses that drains
// (coreAgent.PairedBytesDrainer): its drain notes "drain" in its source's
// told, and counts stopLoss as it drains, the close of a connection the
// application's stop ended that it had not counted before. err is the
// drain's answer.
type drainingWire struct {
	*ownedWire
	stopLoss *coreAgent.OwnedLoss
	err      error
}

func (d *drainingWire) PairedBytesDrain(context.Context) error {
	d.ownedSource.mu.Lock()
	d.told = append(d.told, "drain")
	d.ownedSource.mu.Unlock()
	if d.stopLoss != nil {
		d.put(*d.stopLoss)
		d.stopLoss = nil
	}
	return d.err
}

// The session's last read (UnchargedCaptureLosses) comes after the proxy has
// counted the close of every connection the run's stop ended: it drains first,
// then turns the one-process switch off, then takes and reads. A loss of a
// close the proxy counts only as it drains is the run's, in that read, never
// only the log line of the next session's reset. A drain that fails fails the
// read, with the switch turned off all the same. In a record and a replay, for
// a close keploy cannot account for and for bytes keploy saw lost.
func TestTheSessionsLastReadComesAfterTheProxyCountedTheStopsCloses(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			t.Run(string(mode)+"/"+string(kind), func(t *testing.T) {
				a, src := oneProcessAgent(t, mode)
				loss := oneProcessClose(kind)
				d := &drainingWire{ownedWire: a.Proxy.(*ownedWire), stopLoss: &loss}
				a.Proxy = d
				endPassed(t, a, "TestOne")
				src.toldOf()
				run, err := a.UnchargedCaptureLosses(context.Background())
				require.NoError(t, err)
				told := src.toldOf()
				require.GreaterOrEqual(t, len(told), 3, "told %v", told)
				require.Equal(t, []string{"drain", "off"}, told[:2], "the drain comes first, before the switch is turned off and before any read: %v", told)
				require.True(t, slices.Contains(told[2:], "read"), "the counts are read after the drain: %v", told)
				require.EqualValues(t, 1, kindCount(run, kind), "the close the drain counted is the run's, in the session's last read: %+v", run)
				j, nj := kindOf(windowOf(t, a, "o::TestOne"), kind)
				require.Zero(t, j+nj, "TestOne, ended before the stop, is untouched")
			})
		}
	}

	t.Run("a drain that fails", func(t *testing.T) {
		a, src := oneProcessAgent(t, models.MODE_RECORD)
		a.Proxy = &drainingWire{ownedWire: a.Proxy.(*ownedWire), err: errors.New("keploy's proxy had not finished counting the close of 1 connection(s)")}
		endPassed(t, a, "TestOne")
		src.toldOf()
		_, err := a.UnchargedCaptureLosses(context.Background())
		require.ErrorContains(t, err, "could not count the close of every connection the application's stop ended")
		require.ErrorContains(t, err, "had not finished counting")
		require.Equal(t, []string{"drain", "off"}, src.toldOf(), "a failed drain still turns the switch off, and nothing is read")
	})

	t.Run("a proxy that does not drain", func(t *testing.T) {
		a, _ := oneProcessAgent(t, models.MODE_RECORD)
		require.NoError(t, a.DrainCaptureLosses(context.Background()))
	})
}
