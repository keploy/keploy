package mapdb

import (
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

// maxAliasNodes bounds the nodes aliases may bring into one kept field: a
// field whose aliases expand past it (an alias bomb) is not kept, and keploy
// says so (keepEdits' lost). A field written out without aliases is kept
// whatever its size.
const maxAliasNodes = 10000

// keepEdits is data, a mapping encoded anew, with what a hand edit added to
// old, the file it replaces, that the encoding lacks: each key old has and
// the encoding lacks at a level of the file whose keys keploy knows
// (models.MappingKeys) -- the top, a test entry (matched by its id), a mock
// entry (by its name in its list), a boot, a concurrency group, a suite --
// with its comments, in its place among the keys around it; and old's
// comments on the keys both have. A key keploy knows and left out is
// keploy's to leave out, and is not brought back. The comment old starts
// with is its head, which encodeOver keeps (commentHead). lost names the
// fields it could not keep. data is returned as it is when old has nothing
// to keep, or either does not parse.
func keepEdits(old, data []byte) (out []byte, lost []string) {
	if len(old) == 0 {
		return data, nil
	}
	var od, nd yamlLib.Node
	if yamlLib.Unmarshal(old, &od) != nil || yamlLib.Unmarshal(data, &nd) != nil {
		return data, nil
	}
	oroot, nroot := docRoot(&od), docRoot(&nd)
	if oroot == nil || nroot == nil || oroot.Kind != yamlLib.MappingNode || nroot.Kind != yamlLib.MappingNode {
		return data, nil
	}
	// YAML hangs the comment a file starts with on the document or on its
	// first key: it is the file's head, never that key's.
	od.HeadComment = ""
	if len(oroot.Content) > 0 {
		oroot.Content[0].HeadComment = ""
	}
	m := &merger{}
	if !m.mapping(oroot, nroot, models.MappingLevelTop, "") {
		return data, m.lost
	}
	enc, err := yamlLib.Marshal(&nd)
	if err != nil {
		return data, m.lost
	}
	return enc, m.lost
}

// merger brings old's hand edits into a new encoding (keepEdits).
type merger struct {
	lost []string
}

// childLevel is the level of the items of the list under key, at level, and
// whether there is one: the lists whose items keploy writes as one of its
// types.
func childLevel(level, key string) (string, bool) {
	switch level {
	case models.MappingLevelTop:
		switch key {
		case "tests":
			return models.MappingLevelTest, true
		case "startup", "runner", "worker", "shared":
			return models.MappingLevelEntry, true
		case "boots":
			return models.MappingLevelBoot, true
		case "groups":
			return models.MappingLevelGroup, true
		case "suites":
			return models.MappingLevelSuite, true
		}
	case models.MappingLevelTest:
		if key == "mock_entries" || key == "mocks" {
			return models.MappingLevelEntry, true
		}
	case models.MappingLevelBoot:
		if key == "mocks" {
			return models.MappingLevelEntry, true
		}
	}
	return "", false
}

// identityKeys are the keys an item at level is matched by, between the old
// file and the new encoding.
func identityKeys(level string) []string {
	switch level {
	case models.MappingLevelTest, models.MappingLevelGroup:
		return []string{"id"}
	case models.MappingLevelEntry:
		return []string{"name"}
	case models.MappingLevelBoot:
		return []string{"dir", "key"}
	case models.MappingLevelSuite:
		return []string{"dir", "start"}
	}
	return nil
}

// mapping brings old's hand edits into cur, the mapping at level that old
// was before the rewrite (where names it, for lost), and reports whether
// it changed cur.
func (m *merger) mapping(old, cur *yamlLib.Node, level, where string) bool {
	known := models.MappingKeys(level)
	changed := false
	pos := -1 // the pair of cur after which a kept key goes
	for _, p := range pairsOf(old) {
		k, v := p[0], p[1]
		if j := pairIndex(cur, k.Value); j >= 0 {
			ck, cv := cur.Content[2*j], cur.Content[2*j+1]
			if keepComments(k, ck) || keepComments(resolved(v), cv) {
				changed = true
			}
			if sub, ok := childLevel(level, k.Value); ok && m.list(resolved(v), cv, sub, where+k.Value) {
				changed = true
			}
			pos = j
			continue
		}
		if known[k.Value] {
			continue
		}
		ck, cv, ok := copyPair(k, v)
		if !ok {
			m.lost = append(m.lost, where+k.Value)
			continue
		}
		at := 2 * (pos + 1)
		cur.Content = append(cur.Content[:at], append([]*yamlLib.Node{ck, cv}, cur.Content[at:]...)...)
		pos++
		changed = true
	}
	return changed
}

// list brings old's hand edits into the items of cur, the list at itemLevel
// old was, matching an old item to the new one with its identity
// (identityKeys): the n-th old item of an identity to the n-th new one, so
// an identity the list holds twice (a mock a test lists twice) keeps each
// item's edits on its own item. An item the rewrite dropped takes its edits
// with it.
func (m *merger) list(old, cur *yamlLib.Node, itemLevel, where string) bool {
	if old == nil || cur == nil || old.Kind != yamlLib.SequenceNode || cur.Kind != yamlLib.SequenceNode {
		return false
	}
	keys := identityKeys(itemLevel)
	byID := map[string][]*yamlLib.Node{}
	for _, item := range cur.Content {
		if id := identity(item, keys); id != "" {
			byID[id] = append(byID[id], item)
		}
	}
	changed := false
	nth := map[string]int{}
	for _, item := range old.Content {
		item = resolved(item)
		id := identity(item, keys)
		if id == "" {
			continue
		}
		n := nth[id]
		nth[id]++
		if n >= len(byID[id]) {
			continue
		}
		c := byID[id][n]
		if keepComments(item, c) {
			changed = true
		}
		if m.mapping(item, c, itemLevel, where+"["+id+"].") {
			changed = true
		}
	}
	return changed
}

// identity is the values of keys in the mapping item, joined; "" when item is
// no mapping or holds none of them.
func identity(item *yamlLib.Node, keys []string) string {
	item = resolved(item)
	if item == nil || item.Kind != yamlLib.MappingNode {
		return ""
	}
	var parts []string
	found := false
	for _, k := range keys {
		v := ""
		for _, p := range pairsOf(item) {
			if p[0].Value == k {
				if r := resolved(p[1]); r != nil && r.Kind == yamlLib.ScalarNode {
					v, found = r.Value, true
				}
				break
			}
		}
		parts = append(parts, v)
	}
	if !found {
		return ""
	}
	return strings.Join(parts, " ")
}

// pairsOf is the key and value pairs of the mapping n, as YAML resolves
// them: its own, then those its merge keys (<<) bring that it does not set
// itself (models.MergedMappings).
func pairsOf(n *yamlLib.Node) [][2]*yamlLib.Node {
	n = resolved(n)
	if n == nil || n.Kind != yamlLib.MappingNode {
		return nil
	}
	var out [][2]*yamlLib.Node
	seen := map[string]bool{}
	var merged []*yamlLib.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if k.Value == "<<" {
			merged = append(merged, n.Content[i+1])
			continue
		}
		seen[k.Value] = true
		out = append(out, [2]*yamlLib.Node{k, n.Content[i+1]})
	}
	for _, mm := range models.MergedMappings(merged) {
		for i := 0; i+1 < len(mm.Content); i += 2 {
			if k := mm.Content[i]; k.Value != "<<" && !seen[k.Value] {
				seen[k.Value] = true
				out = append(out, [2]*yamlLib.Node{k, mm.Content[i+1]})
			}
		}
	}
	return out
}

// pairIndex is the index of the pair of the mapping n whose key is key, -1
// when it has none.
func pairIndex(n *yamlLib.Node, key string) int {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return i / 2
		}
	}
	return -1
}

// keepComments gives cur the comments old has where cur has none, and
// reports whether it gave any.
func keepComments(old, cur *yamlLib.Node) bool {
	if old == nil || cur == nil {
		return false
	}
	changed := false
	for _, c := range []struct{ from, to *string }{{&old.HeadComment, &cur.HeadComment}, {&old.LineComment, &cur.LineComment}, {&old.FootComment, &cur.FootComment}} {
		if *c.from != "" && *c.to == "" {
			*c.to, changed = *c.from, true
		}
	}
	return changed
}

// copyPair is a copy of the key and value k and v, each alias in the value
// replaced by a copy of the node it names, without anchors: the new
// encoding holds no anchor an alias could name. ok is false when aliases
// bring more than maxAliasNodes nodes into it.
func copyPair(k, v *yamlLib.Node) (ck, cv *yamlLib.Node, ok bool) {
	budget := maxAliasNodes
	if ck, ok = copyNode(k, &budget, false); !ok {
		return nil, nil, false
	}
	if cv, ok = copyNode(v, &budget, false); !ok {
		return nil, nil, false
	}
	return ck, cv, true
}

// copyNode is a copy of n (copyPair); a node an alias brings in counts
// against budget, one written out does not.
func copyNode(n *yamlLib.Node, budget *int, viaAlias bool) (*yamlLib.Node, bool) {
	if n == nil {
		return nil, true
	}
	if n.Kind == yamlLib.AliasNode {
		if n.Alias == nil {
			return nil, false
		}
		return copyNode(n.Alias, budget, true)
	}
	if viaAlias {
		if *budget--; *budget < 0 {
			return nil, false
		}
	}
	c := *n
	c.Anchor = ""
	c.Content = nil
	for _, child := range n.Content {
		cc, ok := copyNode(child, budget, viaAlias)
		if !ok {
			return nil, false
		}
		c.Content = append(c.Content, cc)
	}
	return &c, true
}

// resolved is n, or the node the alias n names.
func resolved(n *yamlLib.Node) *yamlLib.Node {
	for i := 0; n != nil && n.Kind == yamlLib.AliasNode && i < 64; i++ {
		n = n.Alias
	}
	return n
}

// docRoot is the root node of the document d.
func docRoot(d *yamlLib.Node) *yamlLib.Node {
	if d.Kind == yamlLib.DocumentNode && len(d.Content) == 1 {
		return d.Content[0]
	}
	return nil
}
