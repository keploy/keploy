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

// TestUpdateAssetName pins the self-update asset per platform to the names
// release.yml publishes. macOS is arm64-only: an Intel Mac must get an error
// that says so, not an asset that no longer exists, and the process must exit
// non-zero for it -- as must a platform the update publishes no archive for. The latter is asserted through
// utils.ErrCode because cli/update.go returns nil to cobra on every Update
// error, so that global is the only route to a non-zero exit status.
func TestUpdateAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         string
		wantErr      string
	}{
		{"linux", "amd64", "keploy_linux_amd64.tar.gz", ""},
		{"linux", "arm64", "keploy_linux_arm64.tar.gz", ""},
		{"darwin", "arm64", "keploy_darwin_arm64.tar.gz", ""},
		{"darwin", "amd64", "", "Apple Silicon (arm64) only"},
		{"windows", "amd64", "", "no release archive for windows/amd64"},
	}
	prevErrCode := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = prevErrCode })
	for _, tc := range cases {
		utils.ErrCode = 0
		got, err := updateAssetName(tc.goos, tc.goarch)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("updateAssetName(%q, %q) err = %v, want containing %q", tc.goos, tc.goarch, err, tc.wantErr)
			}
			if got != "" {
				t.Errorf("updateAssetName(%q, %q) = %q, want no URL alongside the error", tc.goos, tc.goarch, got)
			}
			// An Intel Mac is sent to Lima, not Docker: there is no Intel macOS
			// build to update to, and the keploy.io CLI that drives the Docker
			// route on a Mac is arm64-only. Lima runs the Linux build instead.
			if tc.goos == "darwin" && err != nil {
				if !strings.Contains(err.Error(), "inside Lima") || strings.Contains(err.Error(), "Docker") {
					t.Errorf("updateAssetName(%q, %q) err = %v, want it to send an Intel Mac to Lima and not to Docker", tc.goos, tc.goarch, err)
				}
			}
			if utils.ErrCode != utils.ExitUnsupportedPlatform {
				t.Errorf("updateAssetName(%q, %q) left ErrCode = %d, want %d so `keploy update` exits non-zero", tc.goos, tc.goarch, utils.ErrCode, utils.ExitUnsupportedPlatform)
			}
			continue
		}
		if err != nil {
			t.Errorf("updateAssetName(%q, %q) unexpected err: %v", tc.goos, tc.goarch, err)
		}
		if got != tc.want {
			t.Errorf("updateAssetName(%q, %q) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
		if utils.ErrCode != 0 {
			t.Errorf("updateAssetName(%q, %q) set ErrCode = %d on a supported platform", tc.goos, tc.goarch, utils.ErrCode)
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
	err := tools.downloadAndUpdate(context.Background(), zap.NewNop(), srv.URL+"/keploy_darwin_arm64.tar.gz", strings.Repeat("0", 64))
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

// The sha256 checked is GitHub's own digest of the asset; a release without a
// usable one is refused, as is one without the asset.
func TestReleaseAssetSHA256(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	release := utils.GitHubRelease{TagName: "v3.6.200", Assets: []utils.GitHubReleaseAsset{
		{Name: "keploy_linux_amd64.tar.gz", Digest: "sha256:" + strings.ToUpper(sum)},
		{Name: "keploy_linux_arm64.tar.gz"},
		{Name: "keploy_darwin_arm64.tar.gz", Digest: "sha256:" + strings.Repeat("zz", 32)},
		{Name: "keploy_windows_amd64", Digest: "md5:0123"},
	}}
	if got, err := releaseAssetSHA256(release, "keploy_linux_amd64.tar.gz"); err != nil || got != sum {
		t.Errorf("releaseAssetSHA256 = %q, %v; want %q", got, err, sum)
	}
	prevErrCode := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = prevErrCode })
	for _, name := range []string{"keploy_linux_arm64.tar.gz", "keploy_darwin_arm64.tar.gz", "keploy_windows_amd64", "keploy_plan9_amd64.tar.gz"} {
		if got, err := releaseAssetSHA256(release, name); err == nil {
			t.Errorf("releaseAssetSHA256(%s) = %q; want a refusal", name, got)
		}
	}
}

// The asset downloads from the release whose version was announced, not
// latest/, which can move to a newer release in between.
func TestReleaseAssetURL(t *testing.T) {
	if got, want := releaseAssetURL("v3.6.200", "keploy_linux_amd64.tar.gz"), "https://github.com/keploy/keploy/releases/download/v3.6.200/keploy_linux_amd64.tar.gz"; got != want {
		t.Errorf("releaseAssetURL = %q, want %q", got, want)
	}
	if got := releaseAssetURL("../../evil", "a b"); strings.Contains(got, "/../") || strings.Contains(got, " ") {
		t.Errorf("releaseAssetURL does not escape its parts: %q", got)
	}
}
