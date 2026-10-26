package models

import (
	"reflect"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// yamlExtra is the fields of a mapping, a test entry or a mock entry this
// keploy does not know, as they read: key and value nodes in pairs, written
// back after the fields it knows. So a hand edit of mappings.yaml (a note, a
// ticket, a field a newer keploy wrote) survives keploy's rewrites of the
// file. nil when there are none.
type yamlExtra struct{ nodes []*yaml.Node }

// extraFields is the fields of the mapping node n whose keys known does not
// hold, nil when there are none.
func extraFields(n *yaml.Node, known map[string]bool) *yamlExtra {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var x *yamlExtra
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; k.Kind == yaml.ScalarNode && !known[k.Value] && k.Value != "<<" {
			budget := maxDetached
			v := detached(n.Content[i+1], &budget)
			if v == nil {
				continue
			}
			if x == nil {
				x = &yamlExtra{}
			}
			x.nodes = append(x.nodes, k, v)
		}
	}
	return x
}

// maxDetached bounds the nodes one kept field may expand to (detached).
const maxDetached = 10000

// detached is a copy of n with every alias in it replaced by a copy of the
// node it names: keploy re-encodes the fields it knows from their values,
// without their anchors, so an alias a kept field held would name nothing.
// A copy made for an alias drops its anchor. nil when the field expands
// past budget nodes (an alias bomb): it is not kept.
func detached(n *yaml.Node, budget *int) *yaml.Node {
	fromAlias := false
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n, fromAlias = n.Alias, true
	}
	if n == nil {
		return nil
	}
	if *budget--; *budget < 0 {
		return nil
	}
	c := *n
	if fromAlias {
		c.Anchor = ""
	}
	if len(n.Content) > 0 {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			if c.Content[i] = detached(child, budget); c.Content[i] == nil {
				return nil
			}
		}
	}
	return &c
}

// withExtra is v encoded as a mapping node with x's fields after its own,
// or v itself when x has none.
func withExtra(v any, x *yamlExtra) (any, error) {
	if x == nil || len(x.nodes) == 0 {
		return v, nil
	}
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	if n.Kind != yaml.MappingNode {
		return v, nil
	}
	n.Content = append(n.Content, x.nodes...)
	return &n, nil
}

// yamlKeys is the keys the yaml tags of t's fields name, inline structs'
// included: what a decode of t knows.
func yamlKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		name, opts, _ := strings.Cut(tag, ",")
		switch {
		case name == "-":
		case strings.Contains(opts, "inline") && f.Type.Kind() == reflect.Struct:
			for k := range yamlKeys(f.Type) {
				keys[k] = true
			}
		case name != "":
			keys[name] = true
		case f.IsExported():
			keys[strings.ToLower(f.Name)] = true
		}
	}
	return keys
}

var (
	knownOnce                        sync.Once
	mappingKeys, testKeys, entryKeys map[string]bool
)

// known is the keys a mapping, a test entry (in every shape it reads in) and
// a mock entry know.
func known() (mapping, test, entry map[string]bool) {
	knownOnce.Do(func() {
		mappingKeys = yamlKeys(reflect.TypeOf(Mapping{}))
		testKeys = yamlKeys(reflect.TypeOf(writtenTestFormat{}))
		testKeys["mocks"] = true
		entryKeys = yamlKeys(reflect.TypeOf(MockEntry{}))
	})
	return mappingKeys, testKeys, entryKeys
}

// KeepExtra gives m the top-level fields from read and this keploy does not
// know (yamlExtra), in place of its own: a writer that builds a mapping file
// anew from another mapping (a per-test commit) keeps what a hand edit added
// at the top of the file it replaces. A nil from changes nothing.
func (m *Mapping) KeepExtra(from *Mapping) {
	if m == nil || from == nil {
		return
	}
	m.extra = from.extra
}

// KeepTestExtra gives each test entry of m the fields its entry in from has
// and this keploy does not know (yamlExtra), by test id, where from's entry
// has any: a writer that rebuilds a file's test entries from their mocks
// alone (mapdb's Insert) keeps what a hand edit added to each. A nil from
// changes nothing.
func (m *Mapping) KeepTestExtra(from *Mapping) {
	if m == nil || from == nil {
		return
	}
	by := map[string]*yamlExtra{}
	for _, t := range from.TestCases {
		if t.extra != nil {
			by[t.ID] = t.extra
		}
	}
	for i := range m.TestCases {
		if x, ok := by[m.TestCases[i].ID]; ok {
			m.TestCases[i].extra = x
		}
	}
}

// UnmarshalYAML reads a mapping, keeping the fields this keploy does not
// know (yamlExtra).
func (m *Mapping) UnmarshalYAML(n *yaml.Node) error {
	type plain Mapping
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*m = Mapping(p)
	mk, _, _ := known()
	m.extra = extraFields(n, mk)
	return nil
}

// MarshalYAML writes a mapping with the fields it read and this keploy does
// not know after its own.
func (m Mapping) MarshalYAML() (interface{}, error) {
	type plain Mapping
	return withExtra(plain(m), m.extra)
}

// UnmarshalYAML reads a mock entry, keeping the fields this keploy does not
// know (yamlExtra).
func (e *MockEntry) UnmarshalYAML(n *yaml.Node) error {
	type plain MockEntry
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*e = MockEntry(p)
	_, _, ek := known()
	e.extra = extraFields(n, ek)
	return nil
}

// MarshalYAML writes a mock entry with the fields it read and this keploy
// does not know after its own.
func (e MockEntry) MarshalYAML() (interface{}, error) {
	type plain MockEntry
	return withExtra(plain(e), e.extra)
}
