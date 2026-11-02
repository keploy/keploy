package models

import (
	"encoding/json"
	"fmt"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

// Delivery is one deliver: entry of a test's mapping: at replay keploy sends
// the test's recorded incoming request Case to the application once the
// application has been answered the test's mock After. It is how a provider's
// callback (a payment webhook) reaches the application while the provider
// itself is mocked: by event, never by timer.
//
//	deliver:
//	  - case: test-2
//	    after: mock-3
//
// The case is sent as recorded, byte for byte, so a signature the provider
// computed over its body still matches; keploy signs nothing.
//
// Written and read only under the proven switch.
type Delivery struct {
	// Case is one of the test's cases (MappedTestCase.Cases): its recorded
	// request is what is sent.
	Case string `json:"case" yaml:"case"`
	// After is one of the test's HTTP mocks (MappedTestCase.Mocks): the case
	// is sent once that mock's answer was written to the application.
	After string `json:"after" yaml:"after"`
	// Sign is read only to refuse it: keploy sends a case as recorded and
	// signs nothing, so an entry that names sign: (any value but null) is
	// refused at load (DeliverSignUnsupported), never taken and ignored.
	Sign any `json:"sign,omitempty" yaml:"sign,omitempty"`
}

// MaxDeliveriesPerTest bounds the deliver: entries of one test: the replay
// carries each one's recorded request to the agent with the set's table.
const MaxDeliveriesPerTest = 64

// The codes a refused deliver: entry carries (DeliverError.Code), which keploy
// mock check reports as they are.
const (
	DeliverCaseUnknown     = "deliver_case_unknown"
	DeliverAfterUnknown    = "deliver_after_unknown"
	DeliverSignUnsupported = "deliver_sign_unsupported"
	DeliverCaseTwice       = "deliver_case_twice"
	DeliverTooMany         = "deliver_too_many"
)

// DeliverError is a deliver: entry keploy refuses to read: the test's mapping
// is not read with it, never without it (its callback would not be sent and
// its test would fail on a call it never got).
type DeliverError struct {
	Test string
	Code string
	Msg  string
}

func (e *DeliverError) Error() string {
	return fmt.Sprintf("test %q in mappings: deliver: %s", e.Test, e.Msg)
}

// DeliveryCase is a deliver: entry as the replay hands it to the agent
// (ScopeTableReq.Deliver): the entry, its case's recorded request and
// answer, read from the case's file, where the request reached the
// application, and how the replay judges a replayed case's answer. The
// entry's fields sit beside the others on the wire (an embedded struct's
// fields are its own in JSON).
type DeliveryCase struct {
	Delivery
	Req  HTTPReq  `json:"req"`
	Resp HTTPResp `json:"resp"`
	// AppPort is the application's port the case's request reached when it
	// was recorded (TestCase.AppPort). The agent sends the case there, on
	// the application's own address, never to the host its URL names (a
	// tunnel's or an ingress's public name); 0, unknown, is not sent.
	AppPort uint16 `json:"app_port,omitempty"`
	// Noise is the case's own noise (TestCase.Noise).
	Noise map[string][]string `json:"noise,omitempty"`
	// GlobalNoise, IgnoreOrdering and CompareAll are the replay's
	// test.globalNoise.global, test.ignoreOrdering and test.compareAll: the
	// callback's answer is judged as a replayed case's is.
	GlobalNoise    map[string]map[string][]string `json:"global_noise,omitempty"`
	IgnoreOrdering bool                           `json:"ignore_ordering,omitempty"`
	CompareAll     bool                           `json:"compare_all,omitempty"`
}

// errRecordedDeliver is a test entry whose deliver: is not a list of entries.
const errRecordedDeliver = "a test's deliveries (deliver) are not a list of case and after: %w"

// recordedDeliverYAML is a test entry's deliver: under the proven switch. One
// that is not a list of entries is an error, never a reason to read the entry
// without it. With the switch off deliver: is not read.
func recordedDeliverYAML(node *yaml.Node) ([]Delivery, error) {
	if !provenswitch.On() || !nodeHasField(node, "deliver") {
		return nil, nil
	}
	var d struct {
		Deliver []Delivery `yaml:"deliver"`
	}
	if err := node.Decode(&d); err != nil {
		return nil, fmt.Errorf(errRecordedDeliver, err)
	}
	return d.Deliver, nil
}

// recordedDeliverJSON is recordedDeliverYAML for a JSON entry.
func recordedDeliverJSON(data []byte) ([]Delivery, error) {
	if !provenswitch.On() || !jsonContainsField(data, "deliver") {
		return nil, nil
	}
	var d struct {
		Deliver []Delivery `json:"deliver"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf(errRecordedDeliver, err)
	}
	return d.Deliver, nil
}

// ValidateDeliver refuses a test's deliver: entries that keploy could not
// send as written (DeliverError): a case that is not one of the test's
// cases, an after: that is not one of its HTTP mocks (keploy tells only an
// HTTP answer's write), a sign: (keploy signs nothing), a case listed twice,
// or more than MaxDeliveriesPerTest entries.
func ValidateDeliver(tc MappedTestCase) error {
	refuse := func(code, format string, a ...any) error {
		return &DeliverError{Test: tc.ID, Code: code, Msg: fmt.Sprintf(format, a...)}
	}
	if len(tc.Deliver) > MaxDeliveriesPerTest {
		return refuse(DeliverTooMany, "%d entries, more than the %d keploy delivers in one test", len(tc.Deliver), MaxDeliveriesPerTest)
	}
	cases := make(map[string]bool, len(tc.Cases))
	for _, c := range tc.Cases {
		cases[c] = true
	}
	kinds := make(map[string]string, len(tc.Mocks))
	for _, m := range tc.Mocks {
		kinds[m.Name] = m.Kind
	}
	seen := make(map[string]bool, len(tc.Deliver))
	for _, d := range tc.Deliver {
		if !cases[d.Case] {
			return refuse(DeliverCaseUnknown, "case %q is not one of this test's cases; name a case listed under its cases:", d.Case)
		}
		if seen[d.Case] {
			return refuse(DeliverCaseTwice, "case %q is listed twice; keploy delivers a case once", d.Case)
		}
		seen[d.Case] = true
		kind, ok := kinds[d.After]
		if !ok {
			return refuse(DeliverAfterUnknown, "%s's after: %q is not one of this test's mock_entries; set after: to the call that triggers it", d.Case, d.After)
		}
		if kind != string(HTTP) {
			return refuse(DeliverAfterUnknown, "%s's after: %q is not an HTTP call (kind %q); keploy delivers after an HTTP call's answer only", d.Case, d.After, kind)
		}
		if d.Sign != nil {
			return refuse(DeliverSignUnsupported, "%s's sign: is not supported yet; remove it, or keep the body as recorded", d.Case)
		}
	}
	return nil
}
