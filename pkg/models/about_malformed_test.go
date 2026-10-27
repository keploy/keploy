package models

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

// An about block of the wrong shape never fails the read, in either codec:
// it is kept as written, written back unchanged, cloned whole, and read as
// a person's.
func TestAnAboutOfTheWrongShapeIsKeptAsWritten(t *testing.T) {
	cases := []struct{ name, yaml, json string }{
		{"a text", "about: written by our team\n", `{"about":"written by our team"}`},
		{"a list", "about:\n    - body.item\n", `{"about":["body.item"]}`},
		{"by is a list", "about:\n    by:\n        - hand\n", `{"about":{"by":["hand"]}}`},
		{"seen is a text", "about:\n    by: learned\n    seen: some text\n", `{"about":{"by":"learned","seen":"some text"}}`},
	}
	type holder struct {
		About *About `json:"about,omitempty" yaml:"about,omitempty"`
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var fromYAML, fromJSON holder
			if err := yaml.Unmarshal([]byte(c.yaml), &fromYAML); err != nil {
				t.Fatalf("yaml: %v", err)
			}
			if err := json.Unmarshal([]byte(c.json), &fromJSON); err != nil {
				t.Fatalf("json: %v", err)
			}
			for codec, h := range map[string]holder{"yaml": fromYAML, "json": fromJSON} {
				if h.About == nil || h.About.Malformed == nil || h.About.By != "" {
					t.Fatalf("%s: not kept as written: %+v", codec, h.About)
				}
				if s := readAbout(h.About, func(string) string { return "" }); s != AboutHand {
					t.Fatalf("%s: reads as %v, want hand", codec, s)
				}
				if c := h.About.Clone(); c.Malformed == h.About.Malformed || *c.Malformed != *h.About.Malformed {
					t.Fatalf("%s: the clone shares or loses the block", codec)
				}
			}
			y, err := yaml.Marshal(fromYAML)
			if err != nil || string(y) != c.yaml {
				t.Fatalf("yaml rewrite %v:\n%s\nwant\n%s", err, y, c.yaml)
			}
			j, err := json.Marshal(fromJSON)
			if err != nil || string(j) != c.json {
				t.Fatalf("json rewrite %v: %s, want %s", err, j, c.json)
			}
		})
	}
	var a About
	if err := json.Unmarshal([]byte("null"), &a); err != nil || a.Malformed != nil {
		t.Fatalf("a null about is no about: %+v %v", a, err)
	}
}
