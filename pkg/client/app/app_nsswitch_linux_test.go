//go:build linux

package app

import (
	"context"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/internal/nsswitchtest"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The application keploy runs natively sees an /etc/nsswitch.conf of its own
// whose hosts line keploy can see, in keploy record as in keploy test: a
// recording that went by systemd-resolved would hold none of the lookups the
// replay then sends keploy, and the replay would answer them from nothing.
//
// As root, on a host of the test's own with Fedora's hosts line
// (nsswitchtest).
func TestANativeAppSeesItsViewOfNsswitchConfInRecordAndInTest(t *testing.T) {
	const (
		fedora = "passwd:     files systemd\nhosts:      files myhostname resolve [!UNAVAIL=return] dns\n"
		view   = "passwd:     files systemd\nhosts:      files myhostname dns\n"
	)
	nsswitchtest.Host(t, []byte(fedora), func(t *testing.T) {
		for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
			seen := filepath.Join(t.TempDir(), "seen")
			// A cat, which ends by itself: App.Run says no pid to kill a
			// group of.
			a := NewApp(zap.NewNop(), "cat "+nsswitchtest.Path+" > "+seen, nil,
				models.SetupOptions{Mode: mode, CommandType: string(utils.Native)})
			if got := a.Run(context.Background()); got.AppErrorType != models.ErrAppStopped {
				t.Fatalf("%s: the application's run ended %v (%v), want it to have stopped on its own", mode, got.AppErrorType, got.Err)
			}
			if got := nsswitchtest.Read(seen); got != view {
				t.Errorf("in %s, the application saw\n%q\nwant the view\n%q", mode, got, view)
			}
		}
		if got := nsswitchtest.Read(nsswitchtest.Path); got != fedora {
			t.Errorf("the host's %s is %q, want it as it was", nsswitchtest.Path, got)
		}
	})
}
