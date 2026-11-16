package models

import "strings"

// A recorded OP_MSG section is kept as text (MongoOpMessage.Sections), in
// one of two forms: a body section, "{ SectionSingle msg: <document> }",
// and a document sequence, "{ SectionSingle identifier: <name> , msgs: [
// <document>, <document> ] }", each document its extended JSON. These are
// the one reading of those forms that keploy's readers share.
const (
	MongoMsgSectionPrefix = "{ SectionSingle msg: "
	MongoMsgSectionSuffix = " }"

	mongoSeqSectionPrefix = "{ SectionSingle identifier: "
	mongoSeqSectionMsgs   = " , msgs: "
	mongoSeqSectionSuffix = " ] }"
)

// MongoMsgSection returns the document of a recorded body section, its text
// as recorded (not checked to be JSON), and false for a section of any
// other form.
func MongoMsgSection(section string) (string, bool) {
	body, ok := strings.CutPrefix(section, MongoMsgSectionPrefix)
	if !ok {
		return "", false
	}
	if body, ok = strings.CutSuffix(body, MongoMsgSectionSuffix); !ok {
		return "", false
	}
	return body, true
}

// MongoSequenceSection returns the identifier and the documents of a
// recorded document sequence: docs is "[ <document>, ... ]", the documents
// as the text of one JSON array (not checked to be JSON), cut from section
// with no copy. ok is false for a section of any other form.
func MongoSequenceSection(section string) (identifier, docs string, ok bool) {
	rest, ok := strings.CutPrefix(section, mongoSeqSectionPrefix)
	if !ok {
		return "", "", false
	}
	i := strings.Index(rest, mongoSeqSectionMsgs+"[ ")
	if i < 0 {
		return "", "", false
	}
	start := len(mongoSeqSectionPrefix) + i + len(mongoSeqSectionMsgs)
	// The array's "[ " and its " ] }" do not overlap: "[  ] }" is the
	// shortest sequence, one of no document.
	if len(section)-start < len("[ ")+len(mongoSeqSectionSuffix) || !strings.HasSuffix(section, mongoSeqSectionSuffix) {
		return "", "", false
	}
	return rest[:i], section[start : len(section)-len(" }")], true
}
