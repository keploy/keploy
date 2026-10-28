package mapdb

import (
	"fmt"
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
// with is its head, which encodeOver keeps (commentHead). renames is how the
// rewrite renamed the mocks old's mock entries name (EncodeOver): nil when
// each keeps its name. lost names the fields it could not keep (aliases),
// dropped the hand-added fields of old's items the encoding has no item for
// (dropItem). data is returned as it is when old has nothing to keep, or
// either does not parse.
func keepEdits(old, data []byte, renames map[string]string) (out []byte, lost, dropped []string) {
	if len(old) == 0 {
		return data, nil, nil
	}
	var od, nd yamlLib.Node
	if yamlLib.Unmarshal(old, &od) != nil || yamlLib.Unmarshal(data, &nd) != nil {
		return data, nil, nil
	}
	oroot, nroot := docRoot(&od), docRoot(&nd)
	if oroot == nil || nroot == nil || oroot.Kind != yamlLib.MappingNode || nroot.Kind != yamlLib.MappingNode {
		return data, nil, nil
	}
	// YAML hangs the comment a file starts with on the document or on its
	// first key: it is the file's head, never that key's.
	od.HeadComment = ""
	if len(oroot.Content) > 0 {
		oroot.Content[0].HeadComment = ""
	}
	m := &merger{renames: renames, matched: map[*yamlLib.Node]bool{}, kept: map[*yamlLib.Node]bool{}, seen: map[*yamlLib.Node]bool{}}
	changed := m.mapping(oroot, nroot, models.MappingLevelTop, "")
	if m.placeMoved(nroot) {
		changed = true
	}
	if !changed {
		return data, m.lost, m.dropped
	}
	enc, err := yamlLib.Marshal(&nd)
	if err != nil {
		return data, m.lost, m.dropped
	}
	return enc, m.lost, m.dropped
}

// merger brings old's hand edits into a new encoding (keepEdits): renames
// is the rewrite's (EncodeOver), matched the new mock entries an old entry
// matched in its own list and kept those old entries, moved the old entries
// their list lost (placeMoved), and seen the old items matched or lost, so
// an item aliases bring in more than once is lost and said once.
type merger struct {
	renames map[string]string
	lost    []string
	dropped []string
	matched map[*yamlLib.Node]bool
	kept    map[*yamlLib.Node]bool
	moved   []movedEntry
	seen    map[*yamlLib.Node]bool
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
			// A test's list in its older shape, which the rewrite writes
			// as mock_entries (models.MappedTestCase): its entries are
			// matched in the test's own list.
			if level == models.MappingLevelTest && k.Value == "mocks" {
				if j := pairIndex(cur, "mock_entries"); j >= 0 {
					if m.list(resolved(v), cur.Content[2*j+1], models.MappingLevelEntry, where+k.Value) {
						changed = true
					}
					continue
				}
			}
			// Keploy's to leave out; the items of a list go with it (lose).
			if sub, ok := childLevel(level, k.Value); ok {
				m.dropList(resolved(v), sub, where+k.Value)
			}
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
// item's edits on its own item. A mock entry's identity is the name of its
// mock as the rewrite renamed it (entryMatch). An old item with no new item
// in the list is lost (lose).
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
	for i, item := range old.Content {
		item = resolved(item)
		id := identity(item, keys)
		if id == "" {
			m.lose(item, itemLevel, fmt.Sprintf("%s[%d].", where, i))
			continue
		}
		match, ok := id, true
		if itemLevel == models.MappingLevelEntry {
			match, ok = m.entryMatch(id)
		}
		n := nth[match]
		nth[match]++
		if !ok || n >= len(byID[match]) {
			m.lose(item, itemLevel, where+"["+id+"].")
			continue
		}
		c := byID[match][n]
		m.seen[item] = true
		if itemLevel == models.MappingLevelEntry {
			m.matched[c], m.kept[item] = true, true
		}
		if keepComments(item, c) {
			changed = true
		}
		if m.mapping(item, c, itemLevel, where+"["+id+"].") {
			changed = true
		}
	}
	return changed
}

// entryMatch is the name of the mock an old entry naming mock name names in
// the new file, and whether it names one there: name itself without renames
// (each mock keeps its name); with renames, the name the rewrite gave the
// mock, and none when it did not carry it (its call was recorded anew, and
// a new entry that holds the name now is another call).
func (m *merger) entryMatch(name string) (string, bool) {
	if m.renames == nil {
		return name, true
	}
	to, ok := m.renames[name]
	return to, ok
}

// lose is an old item at level, at the place at, that the list it was in
// does not hold any more. A mock entry whose mock the rewrite may still
// name (entryMatch: always without renames) waits for the end
// (placeMoved), which finds it the entry the rewrite moved it to (a test's
// write to worker:), or drops it. Any other item takes its hand edits with
// it, and they are said (dropItem).
func (m *merger) lose(item *yamlLib.Node, level, at string) {
	if item == nil || m.seen[item] {
		return
	}
	m.seen[item] = true
	if level == models.MappingLevelEntry {
		if match, ok := m.entryMatch(identity(item, identityKeys(level))); ok && match != "" {
			m.moved = append(m.moved, movedEntry{item: item, match: match, at: at})
			return
		}
	}
	m.dropItem(item, level, at)
}

// movedEntry is an old mock entry its list does not hold any more, the name
// of its mock in the new file (match), and its place (at).
type movedEntry struct {
	item      *yamlLib.Node
	match, at string
}

// placeMoved gives each old entry its list lost (m.moved), in the old file's
// order, the hand edits of the first entry of the new file, root, that names
// its mock and that no old entry matched in its own list: the entry the
// rewrite moved it to. One with none is dropped (dropItem). It reports
// whether it changed root.
func (m *merger) placeMoved(root *yamlLib.Node) bool {
	if len(m.moved) == 0 {
		return false
	}
	free := map[string][]*yamlLib.Node{}
	var walk func(n *yamlLib.Node, level string)
	walk = func(n *yamlLib.Node, level string) {
		for i := 0; n != nil && n.Kind == yamlLib.MappingNode && i+1 < len(n.Content); i += 2 {
			sub, ok := childLevel(level, n.Content[i].Value)
			if !ok || n.Content[i+1].Kind != yamlLib.SequenceNode {
				continue
			}
			for _, item := range n.Content[i+1].Content {
				if sub == models.MappingLevelEntry && !m.matched[item] {
					if name := identity(item, identityKeys(sub)); name != "" {
						free[name] = append(free[name], item)
					}
				}
				walk(item, sub)
			}
		}
	}
	walk(root, models.MappingLevelTop)
	changed := false
	for _, e := range m.moved {
		if m.kept[e.item] {
			continue // an alias of it is matched in another list
		}
		if len(free[e.match]) == 0 {
			m.dropItem(e.item, models.MappingLevelEntry, e.at)
			continue
		}
		c := free[e.match][0]
		free[e.match] = free[e.match][1:]
		if keepComments(e.item, c) {
			changed = true
		}
		if m.mapping(e.item, c, models.MappingLevelEntry, e.at) {
			changed = true
		}
	}
	return changed
}

// dropList loses each item of old, a list at itemLevel under where that the
// rewrite does not write (lose).
func (m *merger) dropList(old *yamlLib.Node, itemLevel, where string) {
	if old == nil || old.Kind != yamlLib.SequenceNode {
		return
	}
	keys := identityKeys(itemLevel)
	for i, item := range old.Content {
		at := fmt.Sprintf("%s[%d].", where, i)
		if id := identity(item, keys); id != "" {
			at = where + "[" + id + "]."
		}
		m.lose(resolved(item), itemLevel, at)
	}
}

// dropItem adds to m.dropped each field a hand edit added to item, an item
// at level of the old file the rewrite has no item for (a test it removed,
// a mock entry it recorded anew), and loses the items of its lists (lose),
// each field named by where it was (at, the item's place): keploy has
// nowhere to keep it, and says so, so an edit is never lost without a word.
// A field keploy knows is keploy's, and is not named.
func (m *merger) dropItem(item *yamlLib.Node, level, at string) {
	known := models.MappingKeys(level)
	for _, p := range pairsOf(item) {
		k := p[0].Value
		if !known[k] {
			m.dropped = append(m.dropped, at+k)
			continue
		}
		if sub, ok := childLevel(level, k); ok {
			m.dropList(resolved(p[1]), sub, at+k)
		}
	}
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
