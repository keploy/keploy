package mapdb

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

// maxAliasNodes bounds the nodes aliases may bring into one kept field: a
// field whose aliases expand past it (an alias bomb) is not kept, and keploy
// says so (keepEdits' lost). A field written out without aliases is kept
// whatever its size.
const maxAliasNodes = 10000

// extraParses counts the parses of a whole old mapping file a rewrite makes
// past the writer's own decode: keepEdits' own, of a file its writer did not
// hand it parsed, and jsonDropped's. Each writer of a MappingDb hands
// keepEdits the document it decoded its mapping from (decodeInto), and
// decodes no mappings.json it wrote last again (encodeOver): a recording
// rewrites the file on every batch, so its rewrite parses it once. Rows
// hold that.
var extraParses atomic.Int64

// keepEdits is data, a mapping encoded anew, with what a hand edit added to
// old, the file it replaces, that the encoding lacks: each key old has and
// the encoding lacks at a level of the file whose keys keploy knows
// (models.MappingKeys) -- the top, a test entry (matched by its id), a mock
// entry (by its name in its list), a boot, a concurrency group, a suite, a
// value a test gave (by its name) -- with its comments, in its place among
// the keys around it; old's comments on the keys both have (an older test's
// mocks: is its mock_entries:); and the comment old ends with. A key keploy
// knows and left out is keploy's to leave out, and is not brought back. The
// comment old starts with is its head, which encodeOver keeps
// (commentHead). od is the document old parses to when the caller parsed it
// (nil: keepEdits parses old); keepEdits may change it. renames is how the
// rewrite renamed the mocks old's mock entries name (EncodeOver): nil when
// each keeps its name. The Unkept is what it could not keep: the comment
// lines of old the result and old's head do not hold, the fields it could
// not keep (aliases), and the hand-added fields of old's items the encoding
// has no item for (dropItem). data is returned as it is when old holds no
// hand edit (holdsEdits), which costs no parse of data, or when old has
// nothing to keep it in, or either does not parse.
func keepEdits(old []byte, od *yamlLib.Node, data []byte, renames map[string]string) ([]byte, Unkept) {
	if od == nil {
		if len(old) == 0 {
			return data, Unkept{}
		}
		extraParses.Add(1)
		od = &yamlLib.Node{}
		if yamlLib.Unmarshal(old, od) != nil {
			return data, Unkept{}
		}
	}
	oroot := docRoot(od)
	fileHead := ""
	if oroot != nil && oroot.Kind == yamlLib.MappingNode {
		fileHead = takeFileHead(od, oroot, old)
		if !holdsEdits(od, oroot) {
			return data, Unkept{}
		}
	}
	// Each comment line old holds, its head's included: what the result
	// and the head do not hold is dropped, and counted.
	before := commentLines(&yamlLib.Node{HeadComment: fileHead}, commentLines(od, nil))
	kept := headComments(old)
	var nd yamlLib.Node
	if oroot == nil || oroot.Kind != yamlLib.MappingNode || yamlLib.Unmarshal(data, &nd) != nil {
		return data, Unkept{Comments: uncounted(before, kept)}
	}
	nroot := docRoot(&nd)
	if nroot == nil || nroot.Kind != yamlLib.MappingNode {
		return data, Unkept{Comments: uncounted(before, kept)}
	}
	m := &merger{renames: renames, matched: map[*yamlLib.Node]bool{}, kept: map[*yamlLib.Node]bool{}, seen: map[*yamlLib.Node]bool{}}
	changed := m.mapping(oroot, nroot, models.MappingLevelTop, "")
	// YAML hangs the comment a file ends with, after a blank line, on the
	// document: it stays the file's end.
	if od.FootComment != "" && nd.FootComment == "" {
		nd.FootComment, changed = od.FootComment, true
	}
	if m.placeMoved(nroot) {
		changed = true
	}
	unkept := Unkept{Fields: m.lost, Dropped: m.dropped}
	if changed {
		enc, err := yamlLib.Marshal(&nd)
		if err != nil {
			unkept.Comments = uncounted(before, kept)
			return data, unkept
		}
		data, kept = enc, commentLines(&nd, kept)
	}
	unkept.Comments = uncounted(before, kept)
	return data, unkept
}

// takeFileHead takes off the first key of root, the mapping of doc, old's
// document, the lines of the comment above that key that are the file's
// head, and is those lines: YAML hangs the comment a file starts with on
// its document, its mapping and its first key, and that comment is the
// file's, which encodeOver writes as the head (commentHead), not the key's.
// A line is the head's when the head holds it and the comments of doc and
// root do not, each line known as headLine knows it and counted as often as
// it appears; keploy's line is keploy's, which a writer puts where it
// always did (header), and is taken wherever it is. So no line is written
// by both the head and the first key, and a rewrite of a file a rewrite
// wrote changes nothing. A line the head does not hold (after a document
// marker's comment, inside a mapping the file writes as {...}) stays on the
// key. Keploy's line alone, on every file keploy wrote, costs no read of
// old.
func takeFileHead(doc, root *yamlLib.Node, old []byte) string {
	if len(root.Content) == 0 || root.Content[0].HeadComment == "" {
		return ""
	}
	first := root.Content[0]
	c := first.HeadComment
	if strings.HasPrefix(c, versionLine) && !strings.Contains(c, "\n") {
		first.HeadComment = ""
		return c
	}
	held := map[string]int{}
	for _, l := range headComments(old) {
		held[l]++
	}
	for _, l := range strings.Split(doc.HeadComment+"\n"+root.HeadComment, "\n") {
		held[headLine(l)]--
	}
	var took, left []string
	for _, l := range strings.Split(c, "\n") {
		switch n := headLine(l); {
		case n == versionLine:
		case held[n] > 0:
			held[n]--
		default:
			left = append(left, l)
			continue
		}
		took = append(took, l)
	}
	// A blank line beside a line the head took is the head's.
	first.HeadComment = strings.Trim(strings.Join(left, "\n"), "\n")
	return strings.Join(took, "\n")
}

// holdsEdits reports whether doc, a mapping file's document, and root, its
// mapping, the file's head off it (takeFileHead), hold what a hand edit
// adds: a comment past the file's head (commentHead keeps the head, which
// YAML hangs on the document and on the first key), a key keploy does not
// know at a level of the file (models.MappingKeys), an alias, which can bring
// an item of one level into another, or a merge key (<<): keploy writes none
// of them. A file keploy wrote holds none, and keepEdits keeps nothing of it:
// its rewrite is the encoding as it is.
func holdsEdits(doc, root *yamlLib.Node) bool {
	if doc.LineComment != "" || doc.FootComment != "" || nodeEdited(root) {
		return true
	}
	return levelEdited(root, models.MappingLevelTop)
}

// nodeEdited reports whether the node n itself holds a comment or is an
// alias. An anchor no alias names changes nothing a rewrite writes.
func nodeEdited(n *yamlLib.Node) bool {
	return n.HeadComment != "" || n.LineComment != "" || n.FootComment != "" || n.Kind == yamlLib.AliasNode
}

// levelEdited reports whether what n, an item at level of a mapping file
// ("" for a value under one of its keys), holds is a hand edit or holds
// one (holdsEdits); n itself its caller checks (nodeEdited).
func levelEdited(n *yamlLib.Node, level string) bool {
	if level == "" {
		for _, c := range n.Content {
			if nodeEdited(c) || levelEdited(c, "") {
				return true
			}
		}
		return false
	}
	known := models.MappingKeys(level)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if !known[k.Value] || nodeEdited(k) || nodeEdited(v) {
			return true
		}
		sub, ok := childLevel(level, k.Value)
		if !ok || v.Kind != yamlLib.SequenceNode {
			sub = ""
		}
		for _, item := range v.Content {
			if nodeEdited(item) || levelEdited(item, sub) {
				return true
			}
		}
	}
	return false
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
		switch key {
		case "mock_entries", "mocks":
			return models.MappingLevelEntry, true
		case "given":
			return models.MappingLevelGiven, true
		}
	case models.MappingLevelBoot:
		if key == "mocks" {
			return models.MappingLevelEntry, true
		}
	}
	return "", false
}

// identityKeys are the keys an item at level is matched by, between the old
// file and the new encoding. A boot's are its folder and its key, and its key
// names the test it started in and its order among that program's starts
// there (starts.KeyOf: program#n@test), so two tests' boots of one binary in
// one folder never pair with each other; boots of one key (a test run twice
// in one recording) pair in order (list).
func identityKeys(level string) []string {
	switch level {
	case models.MappingLevelTest, models.MappingLevelGroup:
		return []string{"id"}
	case models.MappingLevelEntry, models.MappingLevelGiven:
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
			if keepPairComments(k, resolved(v), ck, cv) {
				changed = true
			}
			if sub, ok := childLevel(level, k.Value); ok {
				if m.list(resolved(v), cv, sub, where+k.Value) {
					changed = true
				}
			} else if keepPlainComments(resolved(v), cv, m.plainMatch(k.Value)) {
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
					// mocks:'s comments go to mock_entries:, as its items do.
					if keepPairComments(k, resolved(v), cur.Content[2*j], cur.Content[2*j+1]) {
						changed = true
					}
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
	nth, listed := map[string]int{}, map[string]int{}
	for i, item := range old.Content {
		item = resolved(item)
		id := identity(item, keys)
		if id == "" {
			m.lose(item, itemLevel, fmt.Sprintf("%s[%d].", where, i))
			continue
		}
		at := nextListing(where, id, listed)
		match, ok := id, true
		if itemLevel == models.MappingLevelEntry {
			match, ok = m.entryMatch(id)
		}
		n := nth[match]
		nth[match]++
		if !ok || n >= len(byID[match]) {
			m.lose(item, itemLevel, at)
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
		if m.mapping(item, c, itemLevel, at) {
			changed = true
		}
	}
	return changed
}

// plainMatch is what names an old value under key in the new file
// (keepPlainComments): a test's case_mocks list its mocks by name, so each
// goes by entryMatch, and a comment never moves to another call that holds
// the name now; any other value is itself. Keploy writes case_mocks at no
// other level: there the key is a person's, and mapping copies it whole.
func (m *merger) plainMatch(key string) func(string) (string, bool) {
	if key == "case_mocks" {
		return m.entryMatch
	}
	return func(v string) (string, bool) { return v, true }
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
	listed := map[string]int{}
	for i, item := range old.Content {
		at := fmt.Sprintf("%s[%d].", where, i)
		if id := identity(item, keys); id != "" {
			at = nextListing(where, id, listed)
		}
		m.lose(resolved(item), itemLevel, at)
	}
}

// nextListing counts the next listing of identity id in the list at where
// (in listed), and is where it is, for a report: [id], and [id #n] for its
// n-th listing there, so a report on a mock a test lists twice says which
// listing.
func nextListing(where, id string, listed map[string]int) string {
	if listed[id]++; listed[id] > 1 {
		return fmt.Sprintf("%s[%s #%d].", where, id, listed[id])
	}
	return where + "[" + id + "]."
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

// keepPlainComments gives cur, the value of a key keploy writes at no level
// of the file, the comments of old, the value it was: each key of a plain map
// (a test's case_mocks, case_steps) keeps those of its pair, wherever the
// encoding puts it, and each list under it those of its items
// (keepItemComments), whose values match names in the new file. A key the
// map no longer holds takes its comments with it, and keepEdits counts them.
// It reports whether it gave any.
func keepPlainComments(old, cur *yamlLib.Node, match func(string) (string, bool)) bool {
	switch {
	case old == nil || old.Kind != cur.Kind:
		return false
	case old.Kind == yamlLib.SequenceNode:
		return keepItemComments(old, cur, match)
	}
	changed := false
	for _, p := range pairsOf(old) {
		j := pairIndex(cur, p[0].Value)
		if j < 0 {
			continue
		}
		v, ck, cv := resolved(p[1]), cur.Content[2*j], cur.Content[2*j+1]
		if keepPairComments(p[0], v, ck, cv) {
			changed = true
		}
		if keepPlainComments(v, cv, match) {
			changed = true
		}
	}
	return changed
}

// keepItemComments gives each plain item of cur, a block list of values
// keploy writes at no level of the file (recordedWith, a test's cases, a
// case's mocks), the comments of the old item whose value match names it:
// the n-th old item of a value to the n-th new one, so a value the list holds
// twice keeps each item's comments on its own item. An old item the list no
// longer holds, or that match names nothing, takes its comments with it, and
// keepEdits counts them. It reports whether it gave any.
func keepItemComments(old, cur *yamlLib.Node, match func(string) (string, bool)) bool {
	byValue := map[string][]*yamlLib.Node{}
	for _, item := range cur.Content {
		if item.Kind == yamlLib.ScalarNode {
			byValue[item.Value] = append(byValue[item.Value], item)
		}
	}
	changed := false
	nth := map[string]int{}
	for _, item := range old.Content {
		if item = resolved(item); item == nil || item.Kind != yamlLib.ScalarNode {
			continue
		}
		value, ok := match(item.Value)
		if !ok {
			continue
		}
		n := nth[value]
		nth[value]++
		if n < len(byValue[value]) && keepComments(item, byValue[value][n]) {
			changed = true
		}
	}
	return changed
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

// keepPairComments gives the pair ck: cv the comments of the pair k: v
// where it has none, the key's and the value's: a comment above a key is
// on the key, one after a scalar or a flow list ([a, b] # note) on the
// value, one after "key:" over a block list on the key. YAML writes the
// comment after a block list or map in its place from the key, and the
// one after a flow list or map from the value; from the other node it
// writes it on a later pair (the next key, or another test's, mock's or
// boot's), or nowhere. A key's comment over a scalar it writes in place
// only while the scalar has none of its own. So the comment after the pair
// goes on the key for a block value and on the value otherwise; when the
// pair had one on each node, the one on the other node is left out, and
// counted (keepEdits).
func keepPairComments(k, v, ck, cv *yamlLib.Node) bool {
	changed := keepComments(k, ck)
	if keepComments(v, cv) {
		changed = true
	}
	if !changed || ck == nil || cv == nil {
		return changed
	}
	from, to := &ck.LineComment, &cv.LineComment
	if (cv.Kind == yamlLib.SequenceNode || cv.Kind == yamlLib.MappingNode) && cv.Style&yamlLib.FlowStyle == 0 {
		from, to = to, from
	}
	if *from != "" {
		if *to == "" {
			*to = *from
		}
		*from = ""
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

// jsonDropped names each field a hand edit added to old, a JSON mapping
// file, at a level of the file whose keys keploy knows
// (models.MappingJSONKeys), as keepEdits names a YAML one, keys in order: a
// JSON rewrite writes only the fields keploy knows, so each is dropped, and
// keploy says so. nil for no old, and for old that does not decode.
func jsonDropped(old []byte) []string {
	if len(old) == 0 {
		return nil
	}
	extraParses.Add(1)
	var doc any
	if json.Unmarshal(old, &doc) != nil {
		return nil
	}
	var out []string
	var walk func(v any, level, at string)
	walk = func(v any, level, at string) {
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		known := models.MappingJSONKeys(level)
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !known[strings.ToLower(k)] {
				out = append(out, at+k)
				continue
			}
			sub, ok := childLevel(level, strings.ToLower(k))
			items, isList := obj[k].([]any)
			if !ok || !isList {
				continue
			}
			listed := map[string]int{}
			for i, item := range items {
				where := fmt.Sprintf("%s[%d].", at+k, i)
				if id := jsonIdentity(item, identityKeys(sub)); id != "" {
					where = nextListing(at+k, id, listed)
				}
				walk(item, sub, where)
			}
		}
	}
	walk(doc, models.MappingLevelTop, "")
	return out
}

// jsonIdentity is identity for an item of a decoded JSON mapping file.
func jsonIdentity(item any, keys []string) string {
	obj, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	found := false
	for _, k := range keys {
		v, isString := obj[k].(string)
		found = found || isString
		parts = append(parts, v)
	}
	if !found {
		return ""
	}
	return strings.Join(parts, " ")
}
