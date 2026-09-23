package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/utils"
)

// TestUpdateDownloadURL pins the self-update asset per platform to the
// names release.yml publishes. macOS is arm64-only: an Intel Mac must get
// an error that says so, not a URL to an asset that no longer exists, and
// the process must exit non-zero for it. The latter is asserted through
// utils.ErrCode because cli/update.go returns nil to cobra on every Update
// error, so that global is the only route to a non-zero exit status.
func TestUpdateDownloadURL(t *testing.T) {
	const base = "https://github.com/keploy/keploy/releases/latest/download/"
	cases := []struct {
		goos, goarch string
		want         string
		wantErr      string
	}{
		{"linux", "amd64", base + "keploy_linux_amd64.tar.gz", ""},
		{"linux", "arm64", base + "keploy_linux_arm64.tar.gz", ""},
		{"darwin", "arm64", base + "keploy_darwin_arm64.tar.gz", ""},
		{"darwin", "amd64", "", "Apple Silicon (arm64) only"},
	}
	prevErrCode := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = prevErrCode })
	for _, tc := range cases {
		utils.ErrCode = 0
		got, err := updateDownloadURL(tc.goos, tc.goarch)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("updateDownloadURL(%q, %q) err = %v, want containing %q", tc.goos, tc.goarch, err, tc.wantErr)
			}
			if got != "" {
				t.Errorf("updateDownloadURL(%q, %q) = %q, want no URL alongside the error", tc.goos, tc.goarch, got)
			}
			// An Intel Mac is sent to Lima, not Docker: there is no Intel macOS
			// build to update to, and the keploy.io CLI that drives the Docker
			// route on a Mac is arm64-only. Lima runs the Linux build instead.
			if tc.goos == "darwin" && err != nil {
				if !strings.Contains(err.Error(), "inside Lima") || strings.Contains(err.Error(), "Docker") {
					t.Errorf("updateDownloadURL(%q, %q) err = %v, want it to send an Intel Mac to Lima and not to Docker", tc.goos, tc.goarch, err)
				}
			}
			if utils.ErrCode != utils.ExitUnsupportedPlatform {
				t.Errorf("updateDownloadURL(%q, %q) left ErrCode = %d, want %d so `keploy update` exits non-zero", tc.goos, tc.goarch, utils.ErrCode, utils.ExitUnsupportedPlatform)
			}
			continue
		}
		if err != nil {
			t.Errorf("updateDownloadURL(%q, %q) unexpected err: %v", tc.goos, tc.goarch, err)
		}
		if got != tc.want {
			t.Errorf("updateDownloadURL(%q, %q) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
		if utils.ErrCode != 0 {
			t.Errorf("updateDownloadURL(%q, %q) set ErrCode = %d on a supported platform", tc.goos, tc.goarch, utils.ErrCode)
		}
	}
}

// A release that does not publish an asset for this platform answers with a
// 404 page, not an archive. That is what every keploy already installed on a
// Mac sees the moment the macOS asset is renamed, so the download must fail
// with the status named and a non-zero exit code -- not io.Copy the error page
// into a .tar.gz and surface as "failed to extract", which cli/update.go logs
// while returning nil to cobra (exit 0).
func TestDownloadAndUpdateRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not Found"))
	}))
	t.Cleanup(srv.Close)

	prevErrCode := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = prevErrCode })
	utils.ErrCode = 0

	tools := &Tools{logger: zap.NewNop()}
	err := tools.downloadAndUpdate(context.Background(), zap.NewNop(), srv.URL+"/keploy_darwin_arm64.tar.gz")
	if err == nil {
		t.Fatal("downloadAndUpdate accepted a 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not name the HTTP status", err)
	}
	if strings.Contains(err.Error(), "extract") {
		t.Errorf("a 404 must not be reported as an extraction failure: %q", err)
	}
	if utils.ErrCode == 0 {
		t.Error("a missing release asset must set a non-zero exit code; cli/update.go returns nil to cobra")
	}
}
