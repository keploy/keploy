package http

import (
	"strings"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/variables"
)

// What says which call a request is. The learn step learns a drifted leaf as
// noise, an echo, an earlier call's value or a read of the clock. Noise is
// never compared again, and an echo answers whatever the call sent, so a
// call to another resource or operation learned either way is answered with
// this one's recording at every later run: a false green. A leaf that names
// the call is therefore learned only where something explains the change: a
// value an earlier call gave (a variable, asserted equal to it at every
// replay) or a read of the application's clock (compared in the call's
// window). Otherwise the call is request_changed, naming the leaf. keploy.yml's
// noise still excuses such a leaf, as the user declared it (Known.Excused
// reads it before any drift).
//
// The classes, and why each names the call:
//   - The method and the destination (the host, or the container) are the
//     operation key itself (operationKey): a recording with another one is
//     never a candidate, so nothing there is learned at all.
//   - Every path segment (url.path.N): a path names the resource a call acts
//     on, and its words the collection, the operation or the item.
//   - The operation headers (operationHeaders): an endpoint that serves many
//     operations at one URL is told which one in a header.
//   - The fields naming the operation or the destination (namingFields),
//     at any depth of a JSON or form body or in the URL query: a protocol
//     that posts every call to one URL names the operation and the resource
//     there, where a REST call's path would.
//
// A leaf no class names keeps the learn step's own rules.

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

// namingFields are the field names, lowercased, that name a call's
// operation or destination where every call goes to one URL: the operation
// of AWS's query protocol (Action), of GraphQL (operationName) and of
// JSON-RPC (method); the resource AWS's JSON and query protocols name in
// the body (a queue, table, topic, stream, log group, state machine, event
// bus, function, secret or key).
var namingFields = map[string]bool{
	"action":             true,
	"operationname":      true,
	"method":             true,
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

// identityClass is the class (above) of the leaf named leaf in keploy.yml's
// noise dialect (learn.Leaf.Noise: "url.path.2", "header.x-amz-target",
// "body.Items[].TableName", "url.query.Action"), as a miss names it; ""
// for a leaf no class names.
func identityClass(leaf string) string {
	switch {
	case strings.HasPrefix(leaf, "url.path."):
		return "a path segment"
	case strings.HasPrefix(leaf, "header."):
		if operationHeaders[strings.TrimPrefix(leaf, "header.")] {
			return "an operation header"
		}
	case strings.HasPrefix(leaf, "body."), strings.HasPrefix(leaf, "url.query."):
		field := leaf[strings.LastIndexByte(leaf, '.')+1:]
		if namingFields[strings.ToLower(field)] {
			return "a field naming the operation or the destination"
		}
	}
	return ""
}

// identityHold is how the learn step may learn each leaf of an HTTP call by
// its class: LearnExplainedOnly for a leaf a class names, LearnAny
// otherwise.
func identityHold(leaf string) LearnHold {
	if identityClass(leaf) != "" {
		return LearnExplainedOnly
	}
	return LearnAny
}

// holdOf is how the learn step may learn the leaf named leaf: as the
// caller of LearnMatch holds it (learnNoise.hold), where it holds it; as
// its class does (identityHold) otherwise, for the HTTP parser's own call
// too.
func (n learnNoise) holdOf(leaf string) LearnHold {
	if n.hold != nil {
		if h := n.hold(leaf); h != LearnAny {
			return h
		}
	}
	return identityHold(leaf)
}

// explained reports whether what the learn step learned at a leaf explains
// the change, as learnPlan carries it: a value an earlier call gave (a
// variable, asserted equal to it at every replay), a value the test gave
// keploy (given: its key's variable, givenFor), or a read of the
// application's clock, with or without an offset (Learned.Clock: compared in
// the call's window). Noise, an echo and anything else explain nothing:
// none says why the call names another resource or operation.
func explained(l learn.Learned, given []variables.Binding) bool {
	if _, ok := givenFor(l.Recorded, l.Live, given); ok {
		return true
	}
	if _, ok := l.Clock(); ok {
		return true
	}
	return l.Kind == learn.KindVariable
}
