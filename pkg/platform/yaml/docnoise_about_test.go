package yaml

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

// A noise.about a person wrote that is not a mapping of paths (a list, a
// text) never fails the block: it is kept as written, in both codecs, and
// written back in its place; an empty about is no about.
func TestANoiseAboutOfTheWrongShapeIsKeptAsWritten(t *testing.T) {
	cases := []struct {
		name, yaml, json string
		kept             bool
	}{
		{"a list", "req:\n    - body.item\nabout:\n    - body.item\n", `{"req":["body.item"],"about":["body.item"]}`, true},
		{"a text", "req:\n    - body.item\nabout: our tracing sets it\n", `{"req":["body.item"],"about":"our tracing sets it"}`, true},
		{"empty", "req: [body.item]\nabout:\n", `{"req":["body.item"],"about":null}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var fromYAML, fromJSON DocNoise
			if err := yamlLib.Unmarshal([]byte(c.yaml), &fromYAML); err != nil {
				t.Fatalf("yaml: %v", err)
			}
			if err := json.Unmarshal([]byte(c.json), &fromJSON); err != nil {
				t.Fatalf("json: %v", err)
			}
			for codec, n := range map[string]DocNoise{"yaml": fromYAML, "json": fromJSON} {
				if len(n.Req) != 1 || n.Req[0] != "body.item" {
					t.Fatalf("%s: the paths were not read: %+v", codec, n)
				}
				if _, ok := n.Other[models.AboutKey]; ok != c.kept || len(n.About) != 0 {
					t.Fatalf("%s: kept=%v, want %v (about %v)", codec, ok, c.kept, n.About)
				}
			}
			if !c.kept {
				return
			}
			y, err := yamlLib.Marshal(fromYAML)
			if err != nil {
				t.Fatal(err)
			}
			if string(y) != c.yaml {
				t.Fatalf("yaml rewrite:\n%s\nwant\n%s", y, c.yaml)
			}
			j, err := json.Marshal(fromJSON)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(j, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(c.json), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("json rewrite: %s, want %s", j, c.json)
			}
		})
	}
}

// While a person's noise.about of the wrong shape stands, an about entry
// beside it is not written: the person's block is, in both codecs.
func TestAKeptNoiseAboutWinsOverEntriesBesideIt(t *testing.T) {
	var n DocNoise
	if err := yamlLib.Unmarshal([]byte("req: [body.item]\nabout: [body.item]\n"), &n); err != nil {
		t.Fatal(err)
	}
	n.About = map[string]models.About{"body.item": {By: models.AboutByLearned, Why: "learned"}}
	y, err := yamlLib.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	for codec, out := range map[string]string{"yaml": string(y), "json": string(j)} {
		if strings.Contains(out, "learned") || !strings.Contains(out, "about") {
			t.Fatalf("%s: %s", codec, out)
		}
	}
}
