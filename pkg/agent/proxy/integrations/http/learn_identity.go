package http

import (
	"mime"
	"slices"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/variables"
)

// What says which call a request is. The learn step learns a drifted leaf as
// noise, an echo, a value the application sent in an earlier call or a read
// of the clock. Noise is never compared again, so a call to another resource
// or operation learned as noise is answered with this one's recording at
// every later run: a false green. A leaf that names the call is therefore
// learned only where something explains the change; otherwise the call is
// request_changed, naming the leaf. keploy.yml's noise still excuses such a
// leaf, as the user declared it (Known.Excused reads it before any drift).
//
// What explains a change there, by the kind of leaf:
//   - A leaf naming the operation (classOperation): a value the application
//     sent in an earlier call (a variable, asserted equal to it at every
//     replay), a value the test gave keploy, or a read of the application's
//     clock, with or without an offset (compared in the call's window).
//     Never an echo: an answer that carries an operation's name back says
//     nothing of which operation the application meant.
//   - A leaf holding the id of what the call acts on (classResource): those,
//     or an echo. An id that drifted between the recording and record's full
//     round and comes back in the answer is the relation an application that
//     mints its ids has with the dependency (PUT /objects/<uuid>), learned
//     as that relation; an id that does not drift is never learned at all.
//
// The classes, and why each names the call:
//   - The method and the destination (the host, or the container) are the
//     operation key itself (operationKey): a recording with another one is
//     never a candidate, so nothing there is learned at all.
//   - Every path segment (url.path.N), a resource: a path names the
//     resource a call acts on, its words the collection and the item.
//   - The operation headers (operationHeaders) and SOAP 1.2's action
//     (soapActionLeaf): an endpoint that serves many operations at one URL
//     is told which one there.
//   - The fields naming the operation (operationFields) or the resource
//     (resourceFields), at any depth of a JSON or form body or in the URL
//     query, an array of them too: a protocol that posts every call to one
//     URL names the operation and the resource there, where a REST call's
//     path would.
//   - A GraphQL document's operations, their types and names, or an
//     unnamed one's root fields (graphQLOperations), in a field named
//     query: a client that sends no operationName says which operation it
//     means only there.
//
// A leaf no class names keeps the learn step's own rules.
//
// The same rule reads noise a recording already holds at such a leaf, an
// earlier keploy's (learnedNoiseHolds): at a leaf holding an id it still
// excuses a change; at one naming the operation it no longer does, at
// replay (strictKnown.noiseHolds) and in record's full round
// (httpKnown.Excused), and the miss says to record the test again
// (staleNoiseWhy).

// The classes of leaf that say which call a request is, as a miss names
// them.
const (
	classPath          = "a path segment"
	classOpHeader      = "an operation header"
	classSOAPAction    = "the SOAP 1.2 action"
	classOpField       = "a field naming the operation"
	classResourceField = "a field naming the resource"
	classGraphQL       = "the GraphQL operation"
)

// classResource reports whether class names a leaf holding the id of what
// the call acts on, which an echo explains; every other class names the
// operation, which only an earlier call's value, the test's or the clock
// explains.
func classResource(class string) bool {
	return class == classPath || class == classResourceField
}

// operationHeaders are the headers, lowercased as httpParts holds them,
// that carry a call's operation: AWS's JSON protocol (X-Amz-Target names
// the service's operation), SOAP 1.1 (SOAPAction), and the method-override
// headers, which tunnel the method the operation key would otherwise hold.
var operationHeaders = map[string]bool{
	"x-amz-target":           true,
	"soapaction":             true,
	"x-http-method-override": true,
	"x-http-method":          true,
	"x-method-override":      true,
}

// soapActionLeaf is the leaf (shape) holding SOAP 1.2's action: the action
// parameter of an application/soap+xml Content-Type, which the media type
// the shape compares drops with every other parameter.
const soapActionLeaf = "header.content-type.action"

// soapAction is Content-Type ct's SOAP 1.2 action, "" where it holds none.
func soapAction(ct string) string {
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || mt != "application/soap+xml" {
		return ""
	}
	return params["action"]
}

// operationFields are the field names, lowercased, that name a call's
// operation where every call goes to one URL: AWS's query protocol
// (Action), GraphQL (operationName) and JSON-RPC (method).
var operationFields = map[string]bool{
	"action":        true,
	"operationname": true,
	"method":        true,
}

// resourceFields are the field names, lowercased, that name the resource a
// call acts on where every call goes to one URL: what AWS's JSON and query
// protocols name in the body (a queue, table, topic, stream, log group,
// state machine, event bus, function, secret or key).
var resourceFields = map[string]bool{
	"queueurl":           true,
	"queuename":          true,
	"tablename":          true,
	"topicarn":           true,
	"targetarn":          true,
	"streamname":         true,
	"streamarn":          true,
	"deliverystreamname": true,
	"loggroupname":       true,
	"logstreamname":      true,
	"statemachinearn":    true,
	"eventbusname":       true,
	"functionname":       true,
	"secretid":           true,
	"keyid":              true,
}

// fieldOf is the field name a body or URL query leaf ends in, without an
// array's brackets ("body.Items[].TableName", "body.QueueUrl[]"): "" for any
// other leaf.
func fieldOf(leaf string) string {
	if !strings.HasPrefix(leaf, "body") && !strings.HasPrefix(leaf, "url.query.") {
		return ""
	}
	field := leaf[strings.LastIndexByte(leaf, '.')+1:]
	if i := strings.IndexByte(field, '['); i >= 0 {
		field = field[:i]
	}
	return field
}

// identityClass is the class (above) of the leaf named leaf in keploy.yml's
// noise dialect (learn.Leaf.Noise: "url.path.2", "header.x-amz-target",
// "body.Items[].TableName", "url.query.Action"), as a miss names it; ""
// for a leaf no class names by its name alone.
func identityClass(leaf string) string {
	switch {
	case strings.HasPrefix(leaf, "url.path."):
		return classPath
	case leaf == soapActionLeaf:
		return classSOAPAction
	case strings.HasPrefix(leaf, "header."):
		if operationHeaders[strings.TrimPrefix(leaf, "header.")] {
			return classOpHeader
		}
	}
	switch field := strings.ToLower(fieldOf(leaf)); {
	case operationFields[field]:
		return classOpField
	case resourceFields[field]:
		return classResourceField
	}
	return ""
}

// identityOf is the class (above) of drifted leaf l (identityLeaf).
func identityOf(l learn.Learned) string {
	return identityLeaf(l.Recorded.Noise, l.Recorded.Value, l.Live.Value)
}

// identityLeaf is the class (above) of the leaf named leaf (identityClass;
// its exact path names it alike) whose value changed from recorded to live:
// by its name or, for a field that may hold a GraphQL document, by what
// changed in it; "" for none. There the operations changed where the two
// values read as different operations (graphQLOperations), a value that
// reads as none reading as "": so also where only one of them reads as
// operations, the field holding an operation on one side and something
// keploy cannot read as one on the other, which fails closed. Only a field
// that reads as no operation on either side (a search text) is no class.
func identityLeaf(leaf, recorded, live string) string {
	if c := identityClass(leaf); c != "" {
		return c
	}
	if graphQLField(leaf) {
		rec, _ := graphQLOperations(recorded)
		now, _ := graphQLOperations(live)
		if rec != now {
			return classGraphQL
		}
	}
	return ""
}

// graphQLField reports whether leaf is a field that may hold a GraphQL
// document (a field named query), whose class only what changed in it says
// (identityLeaf).
func graphQLField(leaf string) bool {
	return strings.EqualFold(fieldOf(leaf), "query")
}

// identityHold is how the learn step may learn a leaf of class class:
// LearnExplainedOrEcho for a leaf holding an id (classResource),
// LearnExplainedOnly for one naming the operation, LearnAny for "".
func identityHold(class string) LearnHold {
	switch {
	case class == "":
		return LearnAny
	case classResource(class):
		return LearnExplainedOrEcho
	}
	return LearnExplainedOnly
}

// learnedNoiseHolds reports whether noise already learned at a leaf of
// class class (identityLeaf), by an earlier keploy, still excuses a change
// there: by the rule the learn step learns by (identityHold), at a leaf
// holding an id (it may learn an echo there, and noise was the echo an
// earlier keploy did not tell apart) and at a leaf of no class; never at a
// leaf naming the operation, which only what explains a change there
// excuses.
func learnedNoiseHolds(class string) bool {
	return noiseExplains(identityHold(class))
}

// noiseExplains reports whether what a replay compares with nothing (noise)
// may explain a change at a leaf the learn step holds hold: anywhere but at
// a leaf naming the operation (LearnExplainedOnly). It is the one rule for
// noise an earlier keploy learned at the leaf itself (learnedNoiseHolds)
// and for a variable whose source leaf is noise (explained): an id that
// noise passes is still one id, named the same wherever the application
// uses it; an operation it passes may be any operation.
func noiseExplains(hold LearnHold) bool {
	return hold != LearnExplainedOnly
}

// staleNoiseWhy is why noise already learned at leaf, its field named shape
// (no value in it), no longer excuses its change from the recording: the
// leaf is of class class, naming the operation (learnedNoiseHolds).
func staleNoiseWhy(shape, leaf, class string) string {
	return shape + " changed, and it says which call this is (" + class + "): the noise an earlier keploy learned there no longer excuses a change, which only a value the application sent in an earlier call at a place keploy compares, a value the test gave keploy, or a read of the clock explains. Record the test again; if the value there may really change, declare it in keploy.yml (" + declareHow(leaf) + ")"
}

// holdOf is how the learn step may learn drifted leaf l: as the caller of
// LearnMatch holds it (learnNoise.hold), where it holds it; as its class
// does (identityOf) otherwise, for the HTTP parser's own call too.
func (n learnNoise) holdOf(l learn.Learned) LearnHold {
	if n.hold != nil {
		if h := n.hold(l.Recorded.Noise); h != LearnAny {
			return h
		}
	}
	return identityHold(identityOf(l))
}

// explained reports whether what the learn step learned at a leaf explains
// the change, as learnPlan carries it: a value the application sent in an
// earlier call (a variable, asserted equal to it at every replay), a value
// the test gave keploy (given: its key's variable, givenFor), or a read of
// the application's clock, with or without an offset (Learned.Clock:
// compared in the call's window). Noise, an echo and anything else explain
// nothing here: an echo explains only an id (heldLearned). At a leaf held
// hold naming the operation (noiseExplains), a variable explains the change
// only where a replay compares the leaf its value came from
// (comparedSource): from a leaf it does not compare, the two values are
// checked only against each other, so together they may name any
// operation. At a leaf holding an id it still does: the application names
// the same id in both calls, whatever the id is.
func explained(l learn.Learned, given []variables.Binding, hold LearnHold) bool {
	if _, ok := givenFor(l.Recorded, l.Live, given); ok {
		return true
	}
	if _, ok := l.Clock(); ok {
		return true
	}
	return l.Kind == learn.KindVariable && (comparedSource(l.From) || noiseExplains(hold))
}

// comparedSource reports whether a replay compares from, the place a
// variable's value came from, with its recording: a leaf of an earlier
// call's request or answer (one with a side) that its call does not leave
// uncompared (learn.Place.Uncompared, noiseMarked). A place in another
// protocol's payload text names no leaf keploy compares by itself, and is
// read as uncompared.
func comparedSource(from learn.Place) bool {
	return from.Side != "" && !from.Uncompared
}

// placeName names from, a variable's source place, for a miss: its mock
// and leaf, or, for a place in its payload text, the field of the mock that
// holds it (learn.Place.Field), the payload alone where it stands across two.
func placeName(from learn.Place) string {
	switch {
	case from.Path != "":
		return from.Mock + " at " + from.Path
	case from.Field != "":
		return from.Mock + "'s payload at " + from.Field
	}
	return from.Mock + "'s payload"
}

// served is e, the exchange a call just served under p, as the tracker
// keeps it for the test's later calls: each request place a variable of
// p's binds or uses named, and each clock read p learned marked
// (learn.Named), then each leaf a replay leaves uncompared marked
// (noiseMarked). The learn step's two serving paths, a call whose
// keploy.yml-declared fields drifted and a paired one, keep it alike.
func (p pairing) served(e learn.Earlier, known learn.Known) learn.Earlier {
	return noiseMarked(learn.Named(e, p.vars, p.clocks), known, p)
}

// noiseMarked is e, the exchange a call just served under p, with each leaf
// a replay leaves uncompared marked so (learn.Seen.Uncompared), whatever
// left it so:
//   - a request leaf the call's learn step learned as noise (p.noise, by
//     its exact path), or that the mock's comparison excuses (known:
//     keploy.yml's noise, the noise learned onto the mock before, an
//     obfuscated value);
//   - each leaf a variable of p's names that takes whatever the
//     application sends: an echo's (models.MockVariable.Echo), bound here,
//     and one whose value came from a place a replay does not compare
//     (p.unchecked), its earlier call's. That is the request leaves it is
//     bound at or used at, and each leaf of the answer it writes that
//     value into (its Answer places). The plan is the only place an echo
//     can be: the learn step never takes a recording that declares
//     variables, which the matcher's variable pass answers.
//
// Every leaf that holds such a value is marked, whichever of them a later
// call's value is found at first (learn's placeRank), so a later call's
// value that follows one explains no change at a leaf naming the operation
// (explained, comparedSource).
func noiseMarked(e learn.Earlier, known learn.Known, p pairing) learn.Earlier {
	asked, answered := map[string]bool{}, map[string]bool{}
	for name, v := range p.vars {
		if !v.Echo && !p.unchecked[name] {
			continue
		}
		for _, place := range append([]string{v.Bind}, v.Use...) {
			if place != "" {
				asked[place] = true
			}
		}
		for _, a := range v.Answer {
			answered[a] = true
		}
	}
	for i, s := range e.Seen {
		switch s.Side {
		case learn.Request:
			if excused, _ := known.Excused(s.Leaf); excused || learnedCovers(p.noise, s.Leaf.Path) || asked[s.Place] {
				e.Seen[i].Uncompared = true
			}
		case learn.Response:
			if answered[s.Place] {
				e.Seen[i].Uncompared = true
			}
		}
	}
	return e
}

// declareHow is the keploy.yml noise that excuses leaf, for a miss to name:
// its bucket (keploy.yml's url, header or requestBody), as the matchers read
// each (requestNoiseFor).
func declareHow(leaf string) string {
	switch {
	case strings.HasPrefix(leaf, "url."):
		return "url noise matching it"
	case strings.HasPrefix(leaf, "header."):
		return "header noise naming the header"
	}
	return "requestBody noise naming the field"
}

// graphQLOperations is the operations GraphQL document doc defines, each
// its type and its name ("query GetOrder"), or, for one with no name (a
// shorthand is a query), its type and the fields it selects at its root
// (graphQLRoots: "query { order }", a fragment's spread there read as the
// fields the fragment selects at its root), in order and joined by commas;
// "" and ok false where doc is no GraphQL document keploy reads whole (a
// search text in a field named query, a brace whose text is no selection
// set, a document with text after its last definition, an operation with no
// name that spreads a fragment the document does not define) or defines no
// operation. A description string may stand before a definition. Fragments
// are no operation, and what a named operation selects is not read.
func graphQLOperations(doc string) (string, bool) {
	type operation struct {
		kw, name string
		// roots: an operation with no name, the selection it opens, as
		// it stands (readGraphQLSelection), read once the document's
		// fragments are known.
		roots []string
	}
	var ops []operation
	frags := map[string]int{} // each fragment's name: where its selection set opens
	i := 0
	for {
		i = skipGraphQLIgnored(doc, i)
		if i >= len(doc) {
			break
		}
		if doc[i] == '{' {
			roots, end, ok := readGraphQLSelection(doc, i)
			if !ok {
				return "", false
			}
			ops, i = append(ops, operation{kw: "query", roots: roots}), end
			continue
		}
		if doc[i] == '"' {
			// A description, which only a definition with a keyword
			// follows (one that does not end reads no keyword after it).
			i = skipGraphQLIgnored(doc, skipGraphQLString(doc, i))
		}
		kw, j := graphQLName(doc, i)
		switch kw {
		case "query", "mutation", "subscription", "fragment":
		default:
			return "", false
		}
		name, k := graphQLName(doc, skipGraphQLIgnored(doc, j))
		if name != "" {
			j = k
		}
		// The selection set: the first brace outside the variables'
		// parentheses, their strings and comments.
		depth := 0
		for j < len(doc) && (doc[j] != '{' || depth > 0) {
			switch doc[j] {
			case '(':
				depth++
			case ')':
				depth--
			case '"':
				j = skipGraphQLString(doc, j)
				continue
			case '#':
				j = skipGraphQLIgnored(doc, j)
				continue
			}
			j++
		}
		op := operation{kw: kw, name: name}
		end, ok := skipGraphQLBlock(doc, j)
		switch {
		case name == "":
			// An operation with no name, or a fragment with none (no
			// valid document): its selection set must read.
			op.roots, end, ok = readGraphQLSelection(doc, j)
		case kw == "fragment":
			frags[name] = j
		}
		if !ok {
			return "", false
		}
		if kw != "fragment" {
			ops = append(ops, op)
		}
		i = end
	}
	read := make([]string, len(ops))
	for k, op := range ops {
		if op.name != "" {
			read[k] = op.kw + " " + op.name
			continue
		}
		roots, ok := graphQLRoots(doc, op.roots, frags)
		if !ok {
			return "", false
		}
		read[k] = op.kw + " " + roots
	}
	return strings.Join(read, ","), len(read) > 0
}

// graphQLRoots is the fields an operation with no name selects at its root,
// sorted and each once ("{ order users }"), from names, the selection it
// opens as it stands: a fragment's spread there ("...F") stands for the
// fields F selects at its root, read at frags[F] (where its selection set
// opens) and spread in turn, each fragment once (a cycle ends there). ok is
// false where a spread names a fragment the document does not define, or
// one whose selection set keploy does not read, or the operation selects no
// field: such a document is no document keploy reads whole (fail closed: it
// may ask for anything).
func graphQLRoots(doc string, names []string, frags map[string]int) (string, bool) {
	var roots []string
	spread := map[string]bool{}
	for queue := slices.Clone(names); len(queue) > 0; queue = queue[1:] {
		f, isSpread := strings.CutPrefix(queue[0], "...")
		if !isSpread {
			roots = append(roots, f)
			continue
		}
		if spread[f] {
			continue
		}
		spread[f] = true
		at, defined := frags[f]
		if !defined {
			return "", false
		}
		inner, _, ok := readGraphQLSelection(doc, at)
		if !ok {
			return "", false
		}
		queue = append(queue, inner...)
	}
	if len(roots) == 0 {
		return "", false
	}
	sort.Strings(roots)
	return "{ " + strings.Join(slices.Compact(roots), " ") + " }", true
}

// readGraphQLSelection reads the selection set that opens at doc[i]: the
// names of the fields it selects, as they stand (a fragment's spread as
// "...Name", an inline fragment's fields as its own), and the index past its
// closing brace. ok is false where its text is no selection set (a Loki
// selector `{app="foo"}`, JSON text), so a value of a field named query that
// only looks like one reads as no operation. An argument list is read as
// balanced parentheses, its values unread.
func readGraphQLSelection(doc string, i int) ([]string, int, bool) {
	if i >= len(doc) || doc[i] != '{' {
		return nil, i, false
	}
	var names []string
	ok := true
	i = skipGraphQLIgnored(doc, i+1)
	for ok && i < len(doc) && doc[i] != '}' {
		if strings.HasPrefix(doc[i:], "...") {
			// A fragment's spread, or an inline fragment (on a type, or
			// on none) with its own selection set, then directives.
			name, j := graphQLName(doc, skipGraphQLIgnored(doc, i+3))
			if name != "" && name != "on" {
				names = append(names, "..."+name)
				i, ok = skipGraphQLDirectives(doc, j)
				continue
			}
			i = skipGraphQLIgnored(doc, i+3)
			if name == "on" {
				var typ string
				if typ, i = graphQLName(doc, skipGraphQLIgnored(doc, j)); typ == "" {
					return nil, i, false
				}
			}
			var inner []string
			if i, ok = skipGraphQLDirectives(doc, i); ok {
				inner, i, ok = readGraphQLSelection(doc, i)
				names, i = append(names, inner...), skipGraphQLIgnored(doc, i)
			}
			continue
		}
		// A field: its alias and a colon, its name, its arguments, its
		// directives, and the selection set it opens.
		name, j := graphQLName(doc, i)
		if k := skipGraphQLIgnored(doc, j); name != "" && k < len(doc) && doc[k] == ':' {
			name, j = graphQLName(doc, skipGraphQLIgnored(doc, k+1))
		}
		if name == "" {
			return nil, j, false
		}
		names = append(names, name)
		i = skipGraphQLIgnored(doc, j)
		if i < len(doc) && doc[i] == '(' {
			i, ok = skipGraphQLPair(doc, i, '(', ')')
		}
		if ok {
			i, ok = skipGraphQLDirectives(doc, i)
		}
		if ok && i < len(doc) && doc[i] == '{' {
			_, i, ok = readGraphQLSelection(doc, i)
			i = skipGraphQLIgnored(doc, i)
		}
	}
	if !ok || i >= len(doc) || len(names) == 0 {
		return nil, i, false
	}
	return names, i + 1, true
}

// skipGraphQLDirectives is the index of the first token past the
// directives (@name, its arguments) at or after doc[i]; ok is false where
// one names nothing or its arguments do not close.
func skipGraphQLDirectives(doc string, i int) (int, bool) {
	ok := true
	for i = skipGraphQLIgnored(doc, i); ok && i < len(doc) && doc[i] == '@'; i = skipGraphQLIgnored(doc, i) {
		var name string
		if name, i = graphQLName(doc, skipGraphQLIgnored(doc, i+1)); name == "" {
			return i, false
		}
		if i = skipGraphQLIgnored(doc, i); i < len(doc) && doc[i] == '(' {
			i, ok = skipGraphQLPair(doc, i, '(', ')')
		}
	}
	return i, ok
}

// skipGraphQLIgnored is the index of doc's first token at or after i,
// past white space, commas, comments and a byte order mark.
func skipGraphQLIgnored(doc string, i int) int {
	for i < len(doc) {
		switch {
		case doc[i] == ' ' || doc[i] == '\t' || doc[i] == '\n' || doc[i] == '\r' || doc[i] == ',':
			i++
		case doc[i] == '#':
			for i < len(doc) && doc[i] != '\n' && doc[i] != '\r' {
				i++
			}
		case strings.HasPrefix(doc[i:], "\xef\xbb\xbf"):
			i += len("\xef\xbb\xbf")
		default:
			return i
		}
	}
	return i
}

// graphQLName is the GraphQL name at doc[i:] and the index past it; "" where
// none starts there.
func graphQLName(doc string, i int) (string, int) {
	j := i
	for j < len(doc) && (doc[j] == '_' || doc[j] >= 'a' && doc[j] <= 'z' || doc[j] >= 'A' && doc[j] <= 'Z' || j > i && doc[j] >= '0' && doc[j] <= '9') {
		j++
	}
	return doc[i:j], j
}

// skipGraphQLString is the index past the string, or block string, that
// starts at doc[i], an escaped quote (\" in a string, \""" in a block
// string) read as part of it; len(doc) where it does not end, where no
// caller then reads a whole document.
func skipGraphQLString(doc string, i int) int {
	if strings.HasPrefix(doc[i:], `"""`) {
		for j := i + 3; j < len(doc); j++ {
			switch {
			case strings.HasPrefix(doc[j:], `\"""`):
				j += 3
			case strings.HasPrefix(doc[j:], `"""`):
				return j + 3
			}
		}
		return len(doc)
	}
	for j := i + 1; j < len(doc); j++ {
		switch doc[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return len(doc)
}

// skipGraphQLBlock is the index past the braces that open at doc[i]
// (skipGraphQLPair).
func skipGraphQLBlock(doc string, i int) (int, bool) {
	return skipGraphQLPair(doc, i, '{', '}')
}

// skipGraphQLPair is the index past the pair, open and shut (braces, or an
// argument list's parentheses), that opens at doc[i], the pairs inside it,
// its strings and its comments read as such; ok is false where doc[i] is no
// open or the pair does not close.
func skipGraphQLPair(doc string, i int, open, shut byte) (int, bool) {
	if i >= len(doc) || doc[i] != open {
		return i, false
	}
	depth := 0
	for i < len(doc) {
		switch doc[i] {
		case open:
			depth++
		case shut:
			depth--
			if depth == 0 {
				return i + 1, true
			}
		case '"':
			i = skipGraphQLString(doc, i)
			continue
		case '#':
			i = skipGraphQLIgnored(doc, i)
			continue
		}
		i++
	}
	return i, false
}
