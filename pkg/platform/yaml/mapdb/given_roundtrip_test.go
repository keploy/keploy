package mapdb

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// A value a test's harness gave (a text the test typed or sent) is any text,
// and mappings.yaml reads it back as written: a text of several lines whose
// first line is indented, or tab-indented, left the file unreadable to its own
// record ("did not find expected key"), which then could not write the set's
// worker scope.
func TestGivenValuesReadBackAsWritten(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	for name, v := range map[string]string{
		"a name":                       "tr-empty-1792953247047",
		"an indented first line":       "   indented first line\nsecond",
		"a tab-indented first line":    "\tstarts with tab\n\tand again",
		"a lone newline":               "\n",
		"a JSON document":              "{\n  \"openapi\": \"3.0.0\"\n}",
		"Windows line ends":            "line one\r\nline two\r\n",
		"a command over several lines": "curl -X POST 'http://localhost/x' \\\n  -d '{\"a\":1}'",
	} {
		t.Run(name, func(t *testing.T) {
			given := []models.GivenValue{{Name: "typed-text-1", Value: v}}
			m := &models.Mapping{Version: string(models.V1Beta1), Kind: models.MappingKind, TestSetID: "s",
				TestCases: []models.MappedTestCase{{ID: "t", Given: given}},
				KeyGiven:  []models.KeyGiven{{Key: "k", Given: given}}}
			enc, err := EncodeMappingF(m, zap.NewNop(), yaml.FormatYAML)
			require.NoError(t, err)
			got, err := DecodeMappingF(enc, zap.NewNop(), yaml.FormatYAML)
			require.NoError(t, err)
			require.Equal(t, given, got.TestCases[0].Given)
			require.Equal(t, given, got.KeyGiven[0].Given)
		})
	}
}
