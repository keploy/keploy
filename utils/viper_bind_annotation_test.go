package utils

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// TestBindFlagsToViper_AnnotatedFlagIsNotBound reproduces the --pass-through-ports
// defect and proves the fix.
//
// BindFlagsToViper derives a command-scoped key <cmd.Name()>.<camelCase(flag)>,
// which assumes the flag's destination lives under the command's own config
// section. --pass-through-ports feeds Agent.PassThroughPorts ([]uint), but on
// any command whose Name() is "record" -- both `keploy record` and
// `keploy mock record` -- the derived key is record.passThroughPorts, which maps
// onto Record.PassThroughPorts ([]models.PassThroughRule): an unrelated
// telemetry-egress feature that merely shares the name. viper.Unmarshal then
// aborts the WHOLE config with
//
//	'record.passThroughPorts[0]' expected a map or struct, got "uint"
//
// taking down both commands that accept the flag. On unpatched code this test
// fails at the first assertion.
func TestBindFlagsToViper_AnnotatedFlagIsNotBound(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := &cobra.Command{Use: "record"}
	cmd.Flags().UintSlice("pass-through-ports", nil, "")
	if err := cmd.Flags().SetAnnotation("pass-through-ports", NoViperBindAnnotation, []string{"true"}); err != nil {
		t.Fatalf("SetAnnotation: %v", err)
	}
	if err := cmd.Flags().Set("pass-through-ports", "3000"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := BindFlagsToViper(zap.NewNop(), cmd, ""); err != nil {
		t.Fatalf("BindFlagsToViper: %v", err)
	}

	if viper.IsSet("record.passThroughPorts") {
		t.Errorf("record.passThroughPorts is bound in viper (value=%v); it collides with "+
			"Record.PassThroughPorts []models.PassThroughRule and aborts viper.Unmarshal",
			viper.Get("record.passThroughPorts"))
	}
	if viper.IsSet("passThroughPorts") {
		t.Errorf("bare passThroughPorts is bound in viper; the annotation did not skip the bind")
	}

	// POSITIVE CASE -- the dangerous regression would be a fix that stops the
	// flag working. Every consumer reads it directly, so it must still be readable.
	got, err := cmd.Flags().GetUintSlice("pass-through-ports")
	if err != nil {
		t.Fatalf("GetUintSlice: %v", err)
	}
	if len(got) != 1 || got[0] != 3000 {
		t.Errorf("flag value lost: got %v, want [3000]", got)
	}
}

// NEGATIVE CONTROL: an un-annotated flag must still bind exactly as before, so
// the fix cannot silently disable viper binding for every other flag.
func TestBindFlagsToViper_UnannotatedFlagStillBound(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := &cobra.Command{Use: "record"}
	cmd.Flags().String("build-delay", "", "")
	if err := cmd.Flags().Set("build-delay", "30"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := BindFlagsToViper(zap.NewNop(), cmd, ""); err != nil {
		t.Fatalf("BindFlagsToViper: %v", err)
	}
	if !viper.IsSet("record.buildDelay") {
		t.Fatal("record.buildDelay is NOT bound; the fix broke normal flag binding")
	}
	if got := viper.GetString("record.buildDelay"); got != "30" {
		t.Errorf("record.buildDelay = %q, want \"30\"", got)
	}
}

// The commands that never had the defect must be unchanged: their Name() differs,
// so their derived key never collided.
func TestBindFlagsToViper_NonCollidingCommandUnaffected(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().UintSlice("pass-through-ports", nil, "")
	if err := cmd.Flags().Set("pass-through-ports", "3000"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := BindFlagsToViper(zap.NewNop(), cmd, ""); err != nil {
		t.Fatalf("BindFlagsToViper: %v", err)
	}
	if !viper.IsSet("test.passThroughPorts") {
		t.Error("test.passThroughPorts not bound; unrelated commands were affected")
	}
}
