package learn

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// wordRecording is an HTTP recording whose request holds a word variable's
// fields: a path segment, a query value, a header, a cookie and a body text,
// beside a JSON key and a query name that end in #<digits>, as a name
// written before word addresses may.
func wordRecording(vars models.MockVariables) *models.Mock {
	return &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq: &models.HTTPReq{
			Method: "PUT",
			URL:    "http://store/apps/app-appRec7?ref=by+appRec7&page%231=appRec7",
			Header: map[string]string{"X-App": "appRec7 v2", "Cookie": "uid=appRec7; theme=dark"},
			Body:   `{"data":{"note":"made by appRec7 at boot"},"a#0":"appRec7","a":"one two"}`,
		},
		Variables: vars,
	}}
}

// A mock's variable place names the leaf the replay reads it at
// (VariableLeafPath): a word address, or a decode step, names its field's
// leaf, never a leaf named "<field>#<n>"; a place the recording holds as
// written (a legacy name ending in #<digits>) names its own leaf; and a
// recording keploy reads no request of reads every place as a word address.
func TestAVariablePlaceNamesTheLeafItsReplayReads(t *testing.T) {
	m := wordRecording(nil)
	for place, want := range map[string]string{
		"url.path[1]#1":            "url.path.1",
		"url.query.ref#1":          "url.query.ref",
		"header.X-App#0":           "header.x-app",
		"header.Cookie|cookie.uid": "header.cookie",
		"body.data.note#2":         "body.data.note",
		"body.data.note":           "body.data.note",
		"body.a#0":                 "body.a#0",
		"url.query.page#1":         "url.query.page#1",
		"body.a#1":                 "body.a",
	} {
		if got, ok := VariableLeafPath(m, place); !ok || got != want {
			t.Errorf("VariableLeafPath(%q) = %q, %v; want %q", place, got, ok, want)
		}
	}
	other := &models.Mock{Name: "mock-2", Kind: models.Mongo}
	if got, ok := VariableLeafPath(other, "body.a#0"); !ok || got != "body.a" {
		t.Errorf("with no request to read: %q, %v; want body.a", got, ok)
	}
	if _, ok := VariableLeafPath(m, "nowhere"); ok {
		t.Error("a string of no place named a leaf")
	}
}

// Learned noise at the field a word variable stands in is dropped, as at a
// whole variable's place: a variable's place is never noise. A place the
// recording holds as written keeps the noise of the field its name begins
// with.
func TestMergeLearnedDropsNoiseAtAWordVariablesField(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	noise := func() models.MockState {
		return models.MockState{ReqBodyNoise: map[string][]string{"body.data.note": nil, "body.a": nil, "body.a#0": nil, "url.path.1": nil}}
	}
	for _, c := range []struct {
		name    string
		m       *models.Mock
		dropped []string
		kept    []string
	}{
		{name: "body word", m: wordRecording(nil), dropped: []string{"body.data.note"}},
		{name: "segment word", m: wordRecording(models.MockVariables{"seg": {Recorded: "appRec7", Use: []string{"url.path[1]#1"}}}), dropped: []string{"url.path.1"}},
		{name: "legacy name", m: wordRecording(models.MockVariables{"legacy": {Recorded: "appRec7", Bind: "body.a#0"}}), dropped: []string{"body.a#0"}, kept: []string{"body.a"}},
		{name: "no request", m: &models.Mock{Name: "mock-1"}, dropped: []string{"body.data.note"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			binds := models.MockVariables{"appId": {Recorded: "appRec7", Bind: "body.data.note#2"}}
			if c.m.Spec.Variables != nil {
				binds = nil
			}
			MergeLearned(c.m, noise(), binds)
			for _, k := range c.dropped {
				if _, ok := c.m.Spec.ReqBodyNoise[k]; ok {
					t.Errorf("noise at %s, a variable's field, was kept: %v", k, c.m.Spec.ReqBodyNoise)
				}
			}
			for _, k := range c.kept {
				if _, ok := c.m.Spec.ReqBodyNoise[k]; !ok {
					t.Errorf("noise at %s, no variable's field, was dropped: %v", k, c.m.Spec.ReqBodyNoise)
				}
			}
		})
	}
}
