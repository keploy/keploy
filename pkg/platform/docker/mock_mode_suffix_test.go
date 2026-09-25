package docker

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestMockModeSuffixForwardsRecordRequests(t *testing.T) {
	for _, tc := range []struct {
		opts models.SetupOptions
		want string
	}{
		{models.SetupOptions{}, ""},
		{models.SetupOptions{MockMode: true}, " --mock-mode"},
		{models.SetupOptions{MockMode: true, RecordRequests: true}, " --mock-mode --record-requests"},
		{models.SetupOptions{RecordRequests: true}, ""},
	} {
		if got := mockModeSuffix(tc.opts); got != tc.want {
			t.Fatalf("%+v: got %q want %q", tc.opts, got, tc.want)
		}
	}
}
