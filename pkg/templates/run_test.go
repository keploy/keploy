package templates

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A value is kept as the exact text it stands for: config.yaml's YAML or
// JSON number 1234567 is 1234567, never 1.234567e+06, so {{int .k}} renders
// the number and not 0.
func TestNewRun_KeepsEachValueAsItsExactText(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{
		"s": "usr_01", "i": 1234567, "big": int64(9007199254740993), "f64": float64(1234567), "frac": 1.5,
		"million": float64(1e6), "num": json.Number("7654321"), "b": true, "nil": nil, "quote": `W/"x1-abc"`,
	}, map[string]interface{}{"pass": "p@ss"})
	assert.Equal(t, map[string]string{
		"s": "usr_01", "i": "1234567", "big": "9007199254740993", "f64": "1234567", "frac": "1.5",
		"million": "1000000", "num": "7654321", "b": "true", "nil": "", "quote": `W/"x1-abc"`,
	}, r.Values())
	assert.True(t, r.Defined(Find("{{string .secret.pass}}")[0]))
	assert.False(t, r.Defined(Find("{{string .secret.nope}}")[0]))
	assert.False(t, r.Empty())
}

// A map or a list in config.yaml or secret.yaml is no value a placeholder can
// stand for. It never stops a run: the key is defined and unusable, and
// only a placeholder that names it is refused (Check), with the key named.
func TestNewRun_AValueThatIsNotScalarIsUnusableNotAnError(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"user": map[string]interface{}{"id": 1}, "id": "usr_1"}, map[string]interface{}{"ids": []interface{}{1}})
	assert.Equal(t, map[string]string{"id": "usr_1"}, r.Values())
	for _, text := range []string{"{{string .user}}", "{{string .secret.ids}}"} {
		p := Find(text)[0]
		assert.True(t, r.Defined(p), text)
		why, bad := r.Unusable(p)
		assert.True(t, bad, text)
		assert.Contains(t, why, `"`+strings.TrimPrefix(p.Key, "secret.")+`"`, "the reason names the key")
	}
	why, _ := r.Unusable(Find("{{string .user}}")[0])
	assert.Contains(t, why, `"user"`)
	assert.Contains(t, why, "map")
	_, bad := r.Unusable(Find("{{string .id}}")[0])
	assert.False(t, bad)
}

// Apply gives captured keys their values, as a pass that skips the test case
// that captured them needs; a key config.yaml does not define is not made.
func TestRun_ApplyGivesEachCapturedKeyItsValue(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"id": "usr_REC", "token": "T0"}, nil)
	r.Apply([]Capture{{Key: "id", Value: "usr_LIVE"}, {Key: "nope", Value: "x"}})
	assert.Equal(t, map[string]string{"id": "usr_LIVE", "token": "T0"}, r.Values())
	r.Reset()
	assert.Equal(t, "usr_REC", r.Values()["id"])
}

// Rule 5: every pass over a set starts each key at its recorded value.
func TestRun_ResetGoesBackToTheRecordedValues(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"token": "T0"}, nil)
	r.Expected(models.HTTPResp{Body: `{"token":"{{capture string .token}}"}`}, models.HTTPResp{Body: `{"token":"T1"}`}, ExpectOptions{})
	require.Equal(t, "T1", r.Values()["token"])
	r.Reset()
	assert.Equal(t, "T0", r.Values()["token"])
}

// Rule 9: template state is the Run's own. Two runs over one test set share
// nothing, and neither writes into the config map it was made from.
func TestRun_EachRunHasItsOwnValues(t *testing.T) {
	config := map[string]interface{}{"id": "usr_REC"}
	a := NewRun(nil, config, nil)
	b := NewRun(nil, config, nil)
	a.Expected(models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`}, models.HTTPResp{Body: `{"id":"usr_A"}`}, ExpectOptions{})
	assert.Equal(t, "usr_A", a.Values()["id"])
	assert.Equal(t, "usr_REC", b.Values()["id"], "another run saw this run's capture")
	assert.Equal(t, map[string]interface{}{"id": "usr_REC"}, config, "a run wrote into config.yaml's map")
}
