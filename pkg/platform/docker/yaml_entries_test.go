package docker

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A mapping's entries are read as compose reads them: its own keys first and
// winning, then those its `<<` merges bring (a list of them in its order,
// and the merges inside them), each key once, aliases followed; neither `<<`
// nor anything of a value that is not a mapping is an entry, and a quoted
// "<<" is an ordinary key.
func TestMappingEntriesReadAMappingAsComposeDoes(t *testing.T) {
	var doc struct {
		M     yaml.Node `yaml:"m"`
		Alias yaml.Node `yaml:"alias"`
		List  yaml.Node `yaml:"list"`
	}
	src := `
a: &a {x: from-a, y: from-a, <<: {n: nested}}
b: &b {x: from-b, z: from-b, w: *a}
k: &k key
m:
  x: own
  <<: [*a, *b]
  "<<": quoted
  *k : aliased-key
alias: *b
list: [*a, plain]
`
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range MappingEntries(&doc.M) {
		v := e.Value.Value
		if e.Value.Kind == yaml.MappingNode {
			v = "{mapping}"
		}
		got = append(got, e.Key.Value+"="+v)
	}
	if want := "x=own <<=quoted key=aliased-key y=from-a n=nested z=from-b w={mapping}"; strings.Join(got, " ") != want {
		t.Fatalf("entries %q; want %q", strings.Join(got, " "), want)
	}
	if e := MappingEntries(&doc.Alias); len(e) != 3 || e[2].Key.Value != "w" || e[2].Value.Kind != yaml.MappingNode {
		t.Fatalf("an aliased mapping's entries %+v; want x, z and w, w's value its anchor's mapping", e)
	}
	if MappingEntries(&doc.List) != nil || MappingEntries(nil) != nil {
		t.Fatal("a sequence (its items never read as keys and values), or no node, has entries")
	}
	if n := AliasTarget(doc.List.Content[0]); n.Kind != yaml.MappingNode || AliasTarget(nil) != nil {
		t.Fatalf("an alias's target %v; want its anchor's mapping", n)
	}
}
