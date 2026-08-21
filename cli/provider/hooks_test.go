package provider

import (
	"context"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// The default must stay exactly the set of platforms with an in-tree
// interception backend. Widening it would let a native command through to the
// hooks stub and fail with a confusing eBPF error instead of a clear one about
// the platform; narrowing it would reject runs that work today.
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
		{"windows", "amd64", true},
		{"windows", "arm64", false}, // falls through to the others stub
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
