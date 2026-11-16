package models

import "strings"

// OutsideTestMiss is a call a test set's application made outside every test
// that no mock answered and that fails nothing (TestReport.OutsideTestMisses),
// as the set's report names it: by its shape, as keploy's log lines name a
// call, since a report may be uploaded. Its path by the shape of its segments
// beside its path tag, its host by the shape of its labels beside its host
// tag (the tag the agent's own lines name it by), an IP address whole, and no
// value the call sent.
type OutsideTestMiss struct {
	Protocol string `json:"protocol" yaml:"protocol"`
	// Call is the call's shape: "GET /{}/{}", "A {}.{}.{}." for a DNS
	// lookup.
	Call    string `json:"call" yaml:"call"`
	PathTag string `json:"pathTag,omitempty" yaml:"path_tag,omitempty"`
	// Destination is the host the call was made to, or the name a DNS
	// lookup asked for, by its shape beside its host tag
	// ("{}.{}.{}:443 #3fa2c1d0e9b8"), an IP address whole.
	Destination string `json:"destination,omitempty" yaml:"destination,omitempty"`
	// When is when the application made it: OutsideBeforeFirstTest,
	// OutsideHeld or OutsideAfterLastTest.
	When string `json:"when" yaml:"when"`
	// Count is how many times the set's run made it.
	Count int `json:"count" yaml:"count"`
	// NextStep is what to do about it, in keploy's words.
	NextStep string `json:"nextStep" yaml:"next_step"`
}

// When an outside-test miss was made (OutsideTestMiss.When).
const (
	// OutsideBeforeFirstTest: as the application started, before the
	// set's first test (UnmatchedCall.BeforeSet).
	OutsideBeforeFirstTest = "before_first_test"
	// OutsideHeld: held by its parser for a later test's recording, which
	// no test of the set answered (UnmatchedCall.Held).
	OutsideHeld = "held_for_a_later_test"
	// OutsideAfterLastTest: after the set's last test.
	OutsideAfterLastTest = "after_last_test"
)

// outsideNextDNS and outsideNextCall are an outside-test miss's next step:
// which call it is, the report and the log line name; what to do is one of
// these.
const (
	outsideNextDNS = "the application looked this name up outside every test, and the recording holds no answer for it, so keploy answered it itself: " +
		"if the application should look it up in its tests' runs too, record the test set again so the recording holds the lookup; " +
		"if the name differs from the recording's (a configuration that differs at replay), make the replay's configuration match the recording's; " +
		"to resolve it for real, set test.allowEgress: [dns] (--allow-egress dns)"
	outsideNextCall = "the application made this call outside every test, and no recording answers it, so keploy answered it itself and relayed nothing: " +
		"if the application should make it in its tests' runs too, record the test set again so the recording holds it; " +
		"if it differs from the recording's (a configuration that differs at replay), make the replay's configuration match the recording's; " +
		"to let it reach its real destination, add a bypassRules entry for its host and port"
)

// OutsideTestMissesOf is calls, the misses a test set's application made
// outside every test that fail nothing, as the set's report names them
// (OutsideTestMiss): each call once, in the order first made, with how many
// times it was made. A connection that sent nothing (MatchPhaseNoRequest)
// asked for nothing and an answered call (Answer) is no miss: both are left
// out. nil for none.
func OutsideTestMissesOf(calls []UnmatchedCall) []OutsideTestMiss {
	var out []OutsideTestMiss
	at := map[OutsideTestMiss]int{}
	for _, c := range calls {
		if c.Answer != "" || c.MatchPhase == MatchPhaseNoRequest {
			continue
		}
		m := outsideTestMissOf(c)
		if i, ok := at[m]; ok {
			out[i].Count++
			continue
		}
		at[m] = len(out)
		m.Count = 1
		out = append(out, m)
	}
	return out
}

// OutsideTestNextStep is what to do about c, a call the application made
// outside every test that no mock answered and that fails nothing: the
// set's report (OutsideTestMiss.NextStep) and its misses file name it.
func OutsideTestNextStep(c UnmatchedCall) string {
	if strings.EqualFold(c.Protocol, "DNS") {
		return outsideNextDNS
	}
	return outsideNextCall
}

// outsideTestMissOf is c as OutsideTestMissesOf names it, with no count.
func outsideTestMissOf(c UnmatchedCall) OutsideTestMiss {
	m := OutsideTestMiss{Protocol: c.Protocol, When: OutsideAfterLastTest, NextStep: OutsideTestNextStep(c)}
	switch {
	case c.BeforeSet:
		m.When = OutsideBeforeFirstTest
	case c.Held:
		m.When = OutsideHeld
	}
	if strings.EqualFold(c.Protocol, "DNS") {
		// The proxy's summary is "<qtype> <name>": the name is what the
		// lookup asked for, a value the application chose. A summary of
		// one word is the name, its type the call's command.
		qtype, name, ok := strings.Cut(strings.TrimSpace(c.ActualSummary), " ")
		if !ok {
			qtype, name = c.Command, qtype
		}
		name = strings.TrimSpace(name)
		m.Call = strings.TrimSpace(qtype + " " + HostShape(name))
		m.Destination = TaggedHostLogName(name, c.HostTag)
		return m
	}
	m.Call = UnmatchedCallLogShape(c)
	m.PathTag = c.PathTag
	m.Destination = TaggedHostLogName(c.Destination, c.HostTag)
	return m
}
