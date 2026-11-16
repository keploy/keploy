package models

import "testing"

// The two forms a recorded OP_MSG section is kept in read back to their
// documents, cut from the section's text as recorded; any other text, or
// one whose frame is cut, reads as neither.
func TestAMongoSectionReadsBackToItsDocuments(t *testing.T) {
	for _, c := range []struct {
		section string
		doc     string
		ok      bool
	}{
		{`{ SectionSingle msg: {"find":"orders"} }`, `{"find":"orders"}`, true},
		{`{ SectionSingle msg: not json }`, `not json`, true},
		{`{ SectionSingle msg: {"find":"orders"}`, "", false},
		{`SectionSingle msg: {"find":"orders"} }`, "", false},
		{`{ SectionSingle identifier: documents , msgs: [ {"a":1} ] }`, "", false},
	} {
		doc, ok := MongoMsgSection(c.section)
		if ok != c.ok || doc != c.doc {
			t.Errorf("MongoMsgSection(%q) = %q, %v; want %q, %v", c.section, doc, ok, c.doc, c.ok)
		}
	}
	for _, c := range []struct {
		section     string
		ident, docs string
		ok          bool
	}{
		{`{ SectionSingle identifier: documents , msgs: [ {"a":1}, {"b":2} ] }`, "documents", `[ {"a":1}, {"b":2} ]`, true},
		{`{ SectionSingle identifier: updates , msgs: [  ] }`, "updates", `[  ]`, true},
		// "[ ] }": the list's "[ " and its " ] }" would overlap.
		{`{ SectionSingle identifier: documents , msgs: [ ] }`, "", "", false},
		{`{ SectionSingle identifier: documents , msgs: [ {"a":1} ]`, "", "", false},
		{`{ SectionSingle identifier: documents msgs: [ {"a":1} ] }`, "", "", false},
		{`{ SectionSingle msg: {"a":1} }`, "", "", false},
		{`SectionSingle identifier: documents , msgs: [ {"a":1} ] }`, "", "", false},
		// Its frame opened by another bracket: no sequence, however long.
		{`[ SectionSingle identifier: documents , msgs: [ {"a":"0123456789012345678901234567890123456789"} ] }`, "", "", false},
	} {
		ident, docs, ok := MongoSequenceSection(c.section)
		if ok != c.ok || ident != c.ident || docs != c.docs {
			t.Errorf("MongoSequenceSection(%q) = %q, %q, %v; want %q, %q, %v", c.section, ident, docs, ok, c.ident, c.docs, c.ok)
		}
	}
}
