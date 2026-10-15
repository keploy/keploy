package yaml

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
	yamlLib "gopkg.in/yaml.v3"
)

// Under the proven switch a path learned with a mask (founder decision 26)
// keeps its patterns on disk, in YAML and JSON, and reads back with them;
// every other path stays a plain path of req.
func TestDocNoiseKeepsAMaskUnderTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const mask = `^order 1234 placed at (?:\d{4}-\d{2}-\d{2})$`
	noise := map[string][]string{"body.note": {mask}, "body.at": {}, "header.x-sent": {mask, `^b$`}}
	doc := NewDocNoise(nil, noise)
	if doc == nil || !reflect.DeepEqual(doc.Req, []string{"body.at", "body.note", "header.x-sent"}) {
		t.Fatalf("req %+v", doc)
	}
	if want := map[string][]string{"body.note": {mask}, "header.x-sent": {mask, `^b$`}}; !reflect.DeepEqual(doc.Mask, want) {
		t.Fatalf("mask %v, want %v", doc.Mask, want)
	}
	y, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var fromY, fromJ DocNoise
	if err := yamlLib.Unmarshal(y, &fromY); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(j, &fromJ); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]*DocNoise{"yaml": &fromY, "json": &fromJ} {
		if got := ResolveReqBodyNoise(d); !reflect.DeepEqual(got, noise) {
			t.Errorf("%s read back %q, want %q", name, got, noise)
		}
	}
	// A mask for a path req does not hold is no noise.
	if got := ResolveReqBodyNoise(&DocNoise{Req: []string{"body.a"}, Mask: map[string][]string{"body.b": {mask}}}); !reflect.DeepEqual(got, map[string][]string{"body.a": {}}) {
		t.Fatalf("resolved %v", got)
	}
}

// Without the switch nothing changes: a path's patterns are not written, and
// a mask on disk is not read (the path is ignored whole, as before).
func TestDocNoiseWithoutTheSwitchIgnoresMasks(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	doc := NewDocNoise(nil, map[string][]string{"body.note": {`^x$`}})
	if doc == nil || doc.Mask != nil || !reflect.DeepEqual(doc.Req, []string{"body.note"}) {
		t.Fatalf("doc %+v", doc)
	}
	y, err := yamlLib.Marshal(doc)
	if err != nil || string(y) != "req:\n    - body.note\n" {
		t.Fatalf("written %q %v", y, err)
	}
	if got := ResolveReqBodyNoise(&DocNoise{Req: []string{"body.note"}, Mask: map[string][]string{"body.note": {`^x$`}}}); !reflect.DeepEqual(got, map[string][]string{"body.note": {}}) {
		t.Fatalf("resolved %v", got)
	}
}
