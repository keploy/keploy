package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// The default must stay exactly the set of platforms with an in-tree
// interception backend, which for this build is Linux and nothing else.
// Widening it would let a native command through to the hooks stub and fail
// with a confusing eBPF error instead of nativeUnsupportedError's clear one;
// narrowing it would reject runs that work today.
//
// windows/amd64 is deliberately false. It was true while an in-tree Windows
// backend existed; that backend now ships in Keploy as installed from
// keploy.io, which widens this predicate from its own init() via
// RegisterNativeCommandSupport. If this row ever flips back to true without a
// backend returning to this repository, native Windows runs would get the stub
// instead of the message pointing at that install.
//
// Taking goos/goarch as parameters is what makes every platform checkable from
// any host — asserting against runtime.GOOS on the CI runner would only ever
// exercise one row.
func TestDefaultNativeCommandSupported(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         bool
	}{
		{"linux", "amd64", true},
		{"linux", "arm64", true},
		{"linux", "386", true},
		{"windows", "amd64", false}, // backend ships in Keploy as installed from keploy.io
		{"windows", "arm64", false},
		{"darwin", "arm64", false},
		{"darwin", "amd64", false},
		{"freebsd", "amd64", false},
	}
	for _, tc := range cases {
		if got := DefaultNativeCommandSupported(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("DefaultNativeCommandSupported(%q, %q) = %v, want %v", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// The seam is only worth having if the gate actually consults it. A test that
// merely assigns the var and reads it back would still pass if someone
// re-inlined the platform condition at the call site and orphaned the var,
// which is the regression this is exposed to.
func TestValidateFlagsConsultsNativeCommandSupport(t *testing.T) {
	cases := []struct {
		name      string
		supported bool
		wantErr   bool
	}{
		{"a build with no backend for this platform rejects a native command", false, true},
		{"a build that reports a backend accepts it", true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := NativeCommandSupported
			t.Cleanup(func() { NativeCommandSupported = original })
			RegisterNativeCommandSupport(func(string, string) bool { return tc.supported })

			cfg := config.New()
			c := NewCmdConfigurator(zap.NewNop(), cfg)
			cmd := &cobra.Command{Use: "record"}
			if err := c.AddFlags(cmd); err != nil {
				t.Fatalf("AddFlags: %v", err)
			}
			if err := cmd.ParseFlags([]string{"-c", "python app.py"}); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			cfg.Command = "python app.py"

			err := c.ValidateFlags(context.Background(), cmd)
			if tc.wantErr {
				if err == nil {
					t.Fatal("ValidateFlags accepted a native command on a build with no interception backend")
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateFlags: %v", err)
			}
			if cfg.CommandType != "native" {
				t.Errorf("cfg.CommandType = %q, want %q — widening the gate must not reclassify the command", cfg.CommandType, "native")
			}
		})
	}
}

func TestRegisterNativeCommandSupportNilRestoresTheDefault(t *testing.T) {
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })

	RegisterNativeCommandSupport(func(string, string) bool { return true })
	if !NativeCommandSupported("darwin", "arm64") {
		t.Fatal("the installed predicate was not used")
	}

	RegisterNativeCommandSupport(nil)
	if NativeCommandSupported("darwin", "arm64") {
		t.Error("a nil predicate must restore the default, which does not support darwin")
	}
}

// The refusal has to send each platform somewhere that works there, and the
// answer differs by architecture: every build that wraps this one shares the
// path, including one that runs natively on Apple Silicon and x86-64 Windows
// but not on an Intel Mac or Windows on ARM. Telling those users to "install
// Keploy, it runs natively there" would send them in a circle.
func TestNativeUnsupportedError(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         []string
		notWant      []string
	}{
		{"darwin", "amd64",
			[]string{"darwin/amd64", "only on Apple Silicon", "inside Lima", "#option-2-install-keploy-with-lima", "Docker"},
			[]string{"install Keploy from", "eBPF"}},
		{"windows", "arm64",
			[]string{"windows/arm64", "only on x86-64", "inside WSL", "windows-wsl"},
			// Docker is not offered: it is untested on Windows on ARM, and the
			// docs send those users to WSL only.
			[]string{"install Keploy from", "Docker", "eBPF"}},
		{"darwin", "arm64",
			[]string{"darwin/arm64", "eBPF", "install Keploy from https://keploy.io/docs/server/installation/", "Docker"},
			[]string{"Lima", "WSL"}},
		{"windows", "amd64",
			[]string{"windows/amd64", "eBPF", "install Keploy from https://keploy.io/docs/server/installation/", "Docker"},
			[]string{"Lima", "WSL"}},
		{"freebsd", "amd64",
			[]string{"freebsd/amd64", "Linux, macOS (Apple Silicon) and Windows (x86-64)", "Docker"},
			[]string{"install Keploy from"}},
	}
	for _, tc := range cases {
		msg := nativeUnsupportedError(tc.goos, tc.goarch).Error()
		for _, w := range tc.want {
			if !strings.Contains(msg, w) {
				t.Errorf("%s/%s: message lacks %q:\n%s", tc.goos, tc.goarch, w, msg)
			}
		}
		for _, w := range tc.notWant {
			if strings.Contains(msg, w) {
				t.Errorf("%s/%s: message should not mention %q:\n%s", tc.goos, tc.goarch, w, msg)
			}
		}
		for _, edition := range []string{"Community", "Enterprise", "OSS"} {
			if strings.Contains(msg, edition) {
				t.Errorf("%s/%s: message names an edition (%q); the product is just \"keploy\":\n%s", tc.goos, tc.goarch, edition, msg)
			}
		}
	}
}
