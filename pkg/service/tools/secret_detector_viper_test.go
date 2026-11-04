package tools

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/pkg/service/tools/internal/secretscan"
)

func corpusFindingsOf(fs []secretscan.Finding) []corpusFinding {
	var out []corpusFinding
	for _, f := range fs {
		out = append(out, corpusFinding{Rule: f.RuleID, Match: f.Match, Secret: f.Secret,
			Line: [2]int{f.StartLine, f.EndLine}, Col: [2]int{f.StartColumn, f.EndColumn}})
	}
	sortCorpusFindings(out)
	return out
}

// THE DETECTOR AND THE CLI KEEP TO THEIR OWN VIPER CONFIGS.
// The CLI keeps its config in viper's global instance: keploy.yml read as
// yml, its flags bound, and KEPLOY_* environment variables bound by
// AutomaticEnv (cli/provider/cmd.go). Rules read into that instance would
// replace the CLI's settings and its config type, and a KEPLOY_RULES
// variable would stand in for the rules. With the global instance set up as
// the CLI sets it up, building the detector must leave every setting and the
// config type as they were, and the detector must find on every corpus text
// what one built beside a clean global instance finds.
func TestNewSecretDetector_KeepsToItsOwnViperConfig(t *testing.T) {
	samples := loadSecretCorpus(t)
	viper.Reset()
	t.Cleanup(viper.Reset)
	clean, err := newSecretDetector()
	if err != nil {
		t.Fatal(err)
	}

	const keployYML = "path: ./keploy\ntest:\n  delay: 7\n"
	viper.SetConfigType("yml")
	if err := viper.ReadConfig(strings.NewReader(keployYML)); err != nil {
		t.Fatal(err)
	}
	viper.AutomaticEnv()
	viper.SetEnvPrefix("KEPLOY")
	t.Setenv("KEPLOY_RULES", "none")
	viper.Set("debug", true)
	before := viper.AllSettings()

	det, err := newSecretDetector()
	if err != nil {
		t.Fatalf("building the detector beside the CLI's viper config: %v", err)
	}
	if after := viper.AllSettings(); !reflect.DeepEqual(after, before) {
		t.Errorf("building the detector changed viper's global config:\nbefore %v\n after %v", before, after)
	}
	if err := viper.ReadConfig(strings.NewReader(keployYML)); err != nil {
		t.Errorf("viper's global config type is no longer yml: %v", err)
	}

	bad := 0
	for _, s := range samples {
		got, want := corpusFindingsOf(det.DetectString(s.Text)), corpusFindingsOf(clean.DetectString(s.Text))
		if slices.Equal(got, want) {
			continue
		}
		bad++
		if bad <= 5 {
			t.Errorf("sample from %s, text %q:\n got %+v\nwant %+v", s.Src, s.Text, got, want)
		}
	}
	if bad > 0 {
		t.Errorf("beside the CLI's viper config, %d of %d samples gave other findings", bad, len(samples))
	}
}
