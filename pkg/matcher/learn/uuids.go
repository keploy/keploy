package learn

import (
	"regexp"
	"strings"
)

// uuidText is a UUID in its canonical text form: 8-4-4-4-12 hex digits, in
// either case.
var uuidText = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// uuidParts reads texts (a value's forms, one side) for the UUIDs they hold
// as a token (tokenAt), lower-cased, and the words of the readings that split
// a UUID into its groups (isWord, wordByte) that the texts hold only as a
// group of one, each with the UUIDs that hold it.
//
// A UUID's groups are no words of their own: two random UUIDs share a group
// of four hex digits by chance (16 bits, and a test holds hundreds of
// UUIDs), and a group they share is no sign that one came from the other. A
// group counts as a word of a value only where it stands outside a UUID too.
func uuidParts(texts []string) (uuids map[string]bool, groups map[string][]string) {
	blanked := make([]string, 0, len(texts))
	for _, text := range texts {
		if strings.Count(text, "-") < 4 {
			blanked = append(blanked, text)
			continue
		}
		blank := []byte(text)
		for _, m := range uuidText.FindAllStringIndex(text, -1) {
			u := text[m[0]:m[1]]
			if !tokenAt(text, u, m[0]) {
				continue
			}
			lu := strings.ToLower(u)
			if uuids == nil {
				uuids, groups = map[string]bool{}, map[string][]string{}
			}
			uuids[lu] = true
			for _, g := range strings.Split(u, "-") {
				if !containsString(groups[g], lu) {
					groups[g] = append(groups[g], lu)
				}
			}
			for k := m[0]; k < m[1]; k++ {
				blank[k] = ' '
			}
		}
		blanked = append(blanked, string(blank))
	}
	if len(groups) == 0 {
		return uuids, nil
	}
	// A group the texts hold outside a UUID too is a word of its own.
	for _, text := range blanked {
		for _, word := range []func(byte) bool{isWord, wordByte} {
			for w := range tokensBy(text, word) {
				delete(groups, w)
			}
		}
	}
	return uuids, groups
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// asWords is what word t of d (recorded, or live) is looked for as, where
// t is no word of its own: the UUIDs d holds it in as a group, when it holds
// it only so (uuidParts); t itself otherwise.
func (d *valueDrift) asWords(t string, recorded bool) (words []string, inUUID bool) {
	groups := d.uuidGroupsLive
	if recorded {
		groups = d.uuidGroupsRec
	}
	if us, ok := groups[t]; ok {
		return us, true
	}
	return []string{t}, false
}

// rawUUIDs are the UUIDs a payload's text holds, in every declared form
// (textForms), lower-cased.
func (dc *DriftCache) rawUUIDs(text string) map[string]bool {
	dc.mu.Lock()
	u, ok := dc.rawUUID[text]
	dc.mu.Unlock()
	if ok {
		return u
	}
	u, _ = uuidParts(textForms(text))
	dc.mu.Lock()
	dc.rawUUID[text] = u
	dc.mu.Unlock()
	return u
}
