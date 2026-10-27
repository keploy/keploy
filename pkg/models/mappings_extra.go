package models

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// yamlExtra is the fields of a mapping, a test entry or a mock entry this
// keploy does not know, as they read: key and value nodes in pairs, written
// back after the fields it knows. So a hand edit of the mappings file (a
// note, a ticket, a field a newer keploy wrote) survives keploy's rewrites of
// it, in YAML (mappings.yaml) and in JSON (mappings.json) alike: a JSON
// file's fields are kept as the nodes YAML reads them as (jsonExtra), and
// written back as JSON (withJSONExtra). nil when there are none.
type yamlExtra struct{ nodes []*yaml.Node }

// extraFields is the fields of the mapping node n whose keys known does not
// hold, nil when there are none.
func extraFields(n *yaml.Node, known map[string]bool) *yamlExtra {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var x *yamlExtra
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		seen[n.Content[i].Value] = true
	}
	keep := func(k, v *yaml.Node) {
		budget := maxDetached
		if v = detached(v, &budget); v == nil {
			return
		}
		if x == nil {
			x = &yamlExtra{}
		}
		x.nodes = append(x.nodes, k, v)
	}
	var merged []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		switch {
		case k.Kind != yaml.ScalarNode:
		case k.Value == "<<":
			merged = append(merged, n.Content[i+1])
		case !known[k.Value]:
			keep(k, n.Content[i+1])
		}
	}
	// The fields a merge (<<) brings that keploy does not know, written out:
	// the node's own fields first, then each merged mapping's in order, as
	// YAML resolves them.
	for _, m := range mergedMappings(merged) {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value != "<<" && !known[k.Value] && !seen[k.Value] {
				seen[k.Value] = true
				c := *k
				c.HeadComment, c.LineComment, c.FootComment = "", "", ""
				keep(&c, m.Content[i+1])
			}
		}
	}
	return x
}

// mergedMappings is the mappings the values of a node's merge keys name: a
// mapping, an alias of one, or a sequence of those, each followed by the
// mappings its own merge keys name, as YAML resolves them; each once.
func mergedMappings(values []*yaml.Node) []*yaml.Node {
	var out []*yaml.Node
	seen := map[*yaml.Node]bool{}
	var add func(*yaml.Node)
	add = func(v *yaml.Node) {
		for v != nil && v.Kind == yaml.AliasNode && v.Alias != nil {
			v = v.Alias
		}
		switch {
		case v == nil || seen[v]:
		case v.Kind == yaml.MappingNode:
			seen[v] = true
			out = append(out, v)
			for i := 0; i+1 < len(v.Content); i += 2 {
				if v.Content[i].Value == "<<" {
					add(v.Content[i+1])
				}
			}
		case v.Kind == yaml.SequenceNode:
			for _, c := range v.Content {
				add(c)
			}
		}
	}
	for _, v := range values {
		add(v)
	}
	return out
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
// or v itself when x has none. A field whose key the YAML form knows (one a
// JSON file kept under a name its YAML form spells as a known key) is not
// written: it would be the key twice.
func withExtra(v any, x *yamlExtra, known map[string]bool) (any, error) {
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
	for i := 0; i+1 < len(x.nodes); i += 2 {
		if !known[x.nodes[i].Value] {
			n.Content = append(n.Content, x.nodes[i], x.nodes[i+1])
		}
	}
	return &n, nil
}

// jsonExtra is the fields of the JSON object data whose keys known does not
// hold, in the order data has them, as the nodes YAML reads them as: the one
// form a mapping keeps unknown fields in, whichever file they came from.
// nil when there are none, or when data is no object.
func jsonExtra(data []byte, known map[string]bool) *yamlExtra {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var x *yamlExtra
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return x
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return x
		}
		if known[key] {
			continue
		}
		// JSON is YAML: the value reads as the node a YAML file would hold.
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) == 0 {
			continue
		}
		if x == nil {
			x = &yamlExtra{}
		}
		v := doc.Content[0]
		defaultStyle(v)
		x.nodes = append(x.nodes, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	}
	return x
}

// defaultStyle gives n and the nodes under it YAML's default style, in place
// of the JSON one (double quotes, flow mappings), so a kept field reads as
// YAML where a YAML file holds it. The encoder still quotes a scalar whose
// plain form would read as another type.
func defaultStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		defaultStyle(c)
	}
}

// withJSONExtra is the JSON object b with x's fields after its own, or b
// itself when x has none. A field whose key the JSON form knows is not
// written, as withExtra does for YAML.
func withJSONExtra(b []byte, x *yamlExtra, known map[string]bool) ([]byte, error) {
	if x == nil || len(x.nodes) == 0 || len(b) < 2 || b[len(b)-1] != '}' {
		return b, nil
	}
	out := append([]byte(nil), b[:len(b)-1]...)
	for i := 0; i+1 < len(x.nodes); i += 2 {
		if known[x.nodes[i].Value] {
			continue
		}
		k, err := json.Marshal(x.nodes[i].Value)
		if err != nil {
			return nil, err
		}
		v, err := nodeJSON(x.nodes[i+1])
		if err != nil {
			return nil, err
		}
		if out[len(out)-1] != '{' {
			out = append(out, ',')
		}
		out = append(append(append(out, k...), ':'), v...)
	}
	return append(out, '}'), nil
}

// nodeJSON is the YAML node n as JSON, a mapping's keys in their order: how a
// kept field is written to a JSON file. A mapping that merges another (<<)
// is written as YAML resolves it, its keys sorted; a scalar JSON cannot hold
// (NaN, an infinity) is written as its text.
func nodeJSON(n *yaml.Node) ([]byte, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return []byte("null"), nil
		}
		return nodeJSON(n.Content[0])
	case yaml.AliasNode:
		if n.Alias == nil {
			return []byte("null"), nil
		}
		return nodeJSON(n.Alias)
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "<<" {
				var v map[string]any
				if err := n.Decode(&v); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			}
		}
		var b bytes.Buffer
		b.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			var key string
			if err := n.Content[i].Decode(&key); err != nil {
				key = n.Content[i].Value
			}
			k, _ := json.Marshal(key)
			b.Write(k)
			b.WriteByte(':')
			v, err := nodeJSON(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			b.Write(v)
		}
		b.WriteByte('}')
		return b.Bytes(), nil
	case yaml.SequenceNode:
		var b bytes.Buffer
		b.WriteByte('[')
		for i, c := range n.Content {
			if i > 0 {
				b.WriteByte(',')
			}
			v, err := nodeJSON(c)
			if err != nil {
				return nil, err
			}
			b.Write(v)
		}
		b.WriteByte(']')
		return b.Bytes(), nil
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return json.Marshal(n.Value)
	}
	if b, err := json.Marshal(v); err == nil {
		return b, nil
	}
	return json.Marshal(n.Value)
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

// jsonKeys is the keys the json tags of t's fields name, embedded structs'
// included: what a JSON decode of t knows.
func jsonKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-":
		case name == "" && f.Anonymous && f.Type.Kind() == reflect.Struct:
			for k := range jsonKeys(f.Type) {
				keys[k] = true
			}
		case name != "":
			keys[name] = true
		case f.IsExported():
			keys[f.Name] = true
		}
	}
	return keys
}

var (
	knownOnce                                    sync.Once
	mappingKeys, testKeys, entryKeys             map[string]bool
	mappingJSONKeys, testJSONKeys, entryJSONKeys map[string]bool
)

func initKnown() {
	knownOnce.Do(func() {
		mappingKeys = yamlKeys(reflect.TypeOf(Mapping{}))
		testKeys = yamlKeys(reflect.TypeOf(writtenTestFormat{}))
		testKeys["mocks"] = true
		entryKeys = yamlKeys(reflect.TypeOf(MockEntry{}))
		mappingJSONKeys = jsonKeys(reflect.TypeOf(Mapping{}))
		testJSONKeys = jsonKeys(reflect.TypeOf(writtenTestFormat{}))
		testJSONKeys["mocks"] = true
		entryJSONKeys = jsonKeys(reflect.TypeOf(MockEntry{}))
	})
}

// known is the keys a mapping, a test entry (in every shape it reads in) and
// a mock entry know, in YAML.
func known() (mapping, test, entry map[string]bool) {
	initKnown()
	return mappingKeys, testKeys, entryKeys
}

// knownJSON is known for JSON, whose keys the fields' json tags name.
func knownJSON() (mapping, test, entry map[string]bool) {
	initKnown()
	return mappingJSONKeys, testJSONKeys, entryJSONKeys
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
// has any, and each of its mock entries those of from's entry of the same
// mock where it has none: a writer that rebuilds a file's test entries from
// their mocks (mapdb's Insert) keeps what a hand edit added to each. A nil
// from changes nothing.
func (m *Mapping) KeepTestExtra(from *Mapping) {
	if m == nil || from == nil {
		return
	}
	by := map[string]*yamlExtra{}
	entries := map[string]map[string]*yamlExtra{}
	for _, t := range from.TestCases {
		if t.extra != nil {
			by[t.ID] = t.extra
		}
		for _, e := range t.Mocks {
			if e.extra != nil {
				if entries[t.ID] == nil {
					entries[t.ID] = map[string]*yamlExtra{}
				}
				entries[t.ID][e.Name] = e.extra
			}
		}
	}
	for i := range m.TestCases {
		t := &m.TestCases[i]
		if x, ok := by[t.ID]; ok {
			t.extra = x
		}
		if old := entries[t.ID]; old != nil {
			// A copy: the entries may be the caller's.
			t.Mocks = slices.Clone(t.Mocks)
			for j := range t.Mocks {
				if x := old[t.Mocks[j].Name]; x != nil && t.Mocks[j].extra == nil {
					t.Mocks[j].extra = x
				}
			}
		}
	}
}

// KeepEntriesExtra is entries with the fields from's entry of the same mock
// has and this keploy does not know (yamlExtra), where an entry has none of
// its own: a writer that replaces a section's entries (startup:, worker:)
// with ones it captured anew keeps what a hand edit added to each. entries
// is not changed; nil stays nil.
func KeepEntriesExtra(entries, from []MockEntry) []MockEntry {
	by := map[string]*yamlExtra{}
	for _, e := range from {
		if e.extra != nil {
			by[e.Name] = e.extra
		}
	}
	if len(by) == 0 || entries == nil {
		return entries
	}
	out := slices.Clone(entries)
	for i := range out {
		if x := by[out[i].Name]; x != nil && out[i].extra == nil {
			out[i].extra = x
		}
	}
	return out
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
	if m.extra != nil && len(n.Content) > 0 && m.extra.nodes[0] == n.Content[0] && n.Content[0].HeadComment != "" {
		// YAML hangs the comment the file starts with on its first field:
		// it is the file's head, never this field's. Under the proven switch
		// the writer keeps it as the head (mapdb's encodeOver); with it off
		// a rewrite keeps no head, as before, rather than move it, a stale
		// version line and all, after the fields keploy knows.
		k := *n.Content[0]
		k.HeadComment = ""
		m.extra.nodes[0] = &k
	}
	return nil
}

// MarshalYAML writes a mapping with the fields it read and this keploy does
// not know after its own.
func (m Mapping) MarshalYAML() (interface{}, error) {
	type plain Mapping
	mk, _, _ := known()
	return withExtra(plain(m), m.extra, mk)
}

// UnmarshalJSON is UnmarshalYAML for a JSON mapping (mappings.json): it
// keeps the fields this keploy does not know.
func (m *Mapping) UnmarshalJSON(data []byte) error {
	type plain Mapping
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*m = Mapping(p)
	mk, _, _ := knownJSON()
	m.extra = jsonExtra(data, mk)
	return nil
}

// MarshalJSON is MarshalYAML for JSON.
func (m Mapping) MarshalJSON() ([]byte, error) {
	type plain Mapping
	b, err := json.Marshal(plain(m))
	if err != nil {
		return nil, err
	}
	mk, _, _ := knownJSON()
	return withJSONExtra(b, m.extra, mk)
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
	_, _, ek := known()
	return withExtra(plain(e), e.extra, ek)
}

// UnmarshalJSON is UnmarshalYAML for a JSON mock entry.
func (e *MockEntry) UnmarshalJSON(data []byte) error {
	type plain MockEntry
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*e = MockEntry(p)
	_, _, ek := knownJSON()
	e.extra = jsonExtra(data, ek)
	return nil
}

// MarshalJSON is MarshalYAML for JSON.
func (e MockEntry) MarshalJSON() ([]byte, error) {
	type plain MockEntry
	b, err := json.Marshal(plain(e))
	if err != nil {
		return nil, err
	}
	_, _, ek := knownJSON()
	return withJSONExtra(b, e.extra, ek)
}
