package docker

import "gopkg.in/yaml.v3"

// YAMLEntry is one key of a YAML mapping and its value, each with its
// aliases followed.
type YAMLEntry struct{ Key, Value *yaml.Node }

// MappingEntries are the entries of the mapping m as compose reads it: its
// own keys first, then the keys it takes through its `<<` merge keys (in
// their order, and through the merges inside what they name) that it does
// not set itself, every key and value with its aliases followed. A `<<` is
// not itself an entry; a quoted "<<" is an ordinary key. nil when m (its
// alias followed) is not a mapping.
//
// A reader that walks only m's own Content misses what compose takes in
// through an anchor: a `depends_on` inherited by `<<: *defaults`, a
// `depends_on: *deps`, a condition merged from a fragment.
func MappingEntries(m *yaml.Node) []YAMLEntry {
	m = aliasTarget(m)
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	var out []YAMLEntry
	seen := map[string]bool{}
	take := func(src *yaml.Node) {
		for i := 0; i+1 < len(src.Content); i += 2 {
			key := aliasTarget(src.Content[i])
			if isMergeKey(src.Content[i]) || seen[key.Value] {
				continue
			}
			seen[key.Value] = true
			out = append(out, YAMLEntry{Key: key, Value: aliasTarget(src.Content[i+1])})
		}
	}
	take(m)
	for i := 0; i+1 < len(m.Content); i += 2 {
		if !isMergeKey(m.Content[i]) {
			continue
		}
		sources, _ := mergeSources(m.Content[i+1])
		for _, src := range sources {
			take(src)
		}
	}
	return out
}

// AliasTarget is the node n names, its aliases followed; n itself when it is
// not an alias.
func AliasTarget(n *yaml.Node) *yaml.Node { return aliasTarget(n) }
