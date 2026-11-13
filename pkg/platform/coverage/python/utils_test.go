package python

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// iniSections groups each non-blank line of an INI file under the section
// header above it. Keploy adds its settings in map order, so a section's lines
// are compared as a set.
func iniSections(cfg string) map[string][]string {
	sections := map[string][]string{}
	current := ""
	for _, line := range strings.Split(cfg, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]"):
			current = trimmed
		default:
			sections[current] = append(sections[current], line)
		}
	}
	return sections
}

func TestCreatePyCoverageConfig(t *testing.T) {
	keployRunSettings := []string{
		"sigterm = true",
		"concurrency = multiprocessing, thread",
		"parallel = true",
		"data_file = .coverage.keploy",
	}
	defaultConfig := map[string][]string{
		"[run]": append([]string{"omit =", "    /usr/*"}, keployRunSettings...),
	}
	tests := []struct {
		name     string
		existing *string
		want     map[string][]string
	}{
		{
			name: "no .coveragerc gets the default",
			want: defaultConfig,
		},
		{
			name:     "empty .coveragerc gets the default",
			existing: new(""),
			want:     defaultConfig,
		},
		{
			name:     "existing [run] section keeps user settings and gains the missing keploy keys",
			existing: new("[run]\nsource = app\nbranch = true\nparallel = false\n\n[report]\nshow_missing = true\n"),
			want: map[string][]string{
				"[run]": {
					"source = app",
					"branch = true",
					"parallel = true",
					"sigterm = true",
					"concurrency = multiprocessing, thread",
					"data_file = .coverage.keploy",
				},
				"[report]": {"show_missing = true"},
			},
		},
		{
			name:     "config without a [run] section gains one",
			existing: new("[report]\nshow_missing = true\n"),
			want: map[string][]string{
				"[run]":    keployRunSettings,
				"[report]": {"show_missing = true"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tt.existing != nil {
				require.NoError(t, os.WriteFile(".coveragerc", []byte(*tt.existing), 0o644))
			}

			createPyCoverageConfig(zap.NewNop())

			written, err := os.ReadFile(".coveragerc")
			require.NoError(t, err)
			got := iniSections(string(written))
			require.Len(t, got, len(tt.want), "sections in:\n%s", written)
			for section, lines := range tt.want {
				require.ElementsMatch(t, lines, got[section], "section %s in:\n%s", section, written)
			}
		})
	}
}
