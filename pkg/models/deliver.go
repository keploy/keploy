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
//	    sign: {scheme: hmac-sha256-hex, header: X-Pay-Signature, secret_env: PAY_WEBHOOK_SECRET}
//
// Written and read only under the proven switch.
type Delivery struct {
	// Case is one of the test's cases (MappedTestCase.Cases): its recorded
	// request is what is sent.
	Case string `json:"case" yaml:"case"`
	// After is one of the test's HTTP mocks (MappedTestCase.Mocks): the case
	// is sent once that mock's answer was written to the application.
	After string `json:"after" yaml:"after"`
	// Sign says how to sign a body that differs from the recording. Without
	// it such a body is not sent (its signature would not match).
	Sign *SignSpec `json:"sign,omitempty" yaml:"sign,omitempty"`
}

// SignSpec is how a delivered body is signed again when it differs from the
// recording: the scheme, the header that carries the signature, and the
// environment variable that holds the secret. keploy reads the secret at
// replay and never stores it.
type SignSpec struct {
	Scheme    string `json:"scheme" yaml:"scheme"`
	Header    string `json:"header" yaml:"header"`
	SecretEnv string `json:"secret_env" yaml:"secret_env"`
}

// The signing schemes a deliver: entry may name.
const (
	// SignHMACSHA256Hex is the hex HMAC-SHA256 of the body.
	SignHMACSHA256Hex = "hmac-sha256-hex"
	// SignStripeV1 is Stripe's t=<unix>,v1=<hex HMAC-SHA256 of "t.body">.
	SignStripeV1 = "stripe-v1"
	// SignRazorpay is Razorpay's hex HMAC-SHA256 of the body.
	SignRazorpay = "razorpay"
	// SignGitHubSHA256 is GitHub's sha256=<hex HMAC-SHA256 of the body>.
	SignGitHubSHA256 = "github-sha256"
)

// SignSchemes are the schemes a deliver: entry may name, in the order the
// refusal of another lists them.
var SignSchemes = []string{SignHMACSHA256Hex, SignStripeV1, SignRazorpay, SignGitHubSHA256}

// MaxDeliveriesPerTest bounds the deliver: entries of one test: the replay
// carries each one's recorded request to the agent with the set's table.
const MaxDeliveriesPerTest = 64

// The codes a refused deliver: entry carries (DeliverError.Code), which keploy
// mock check reports as they are.
const (
	DeliverCaseUnknown  = "deliver_case_unknown"
	DeliverAfterUnknown = "deliver_after_unknown"
	DeliverSignInvalid  = "deliver_sign_invalid"
	DeliverCaseTwice    = "deliver_case_twice"
	DeliverTooMany      = "deliver_too_many"
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
// (ScopeTableReq.Deliver): the entry, and its case's recorded request and
// answer, read from the case's file. The entry's fields sit beside req and
// resp on the wire (an embedded struct's fields are its own in JSON).
type DeliveryCase struct {
	Delivery
	Req  HTTPReq  `json:"req"`
	Resp HTTPResp `json:"resp"`
}

// errRecordedDeliver is a test entry whose deliver: is not a list of entries.
const errRecordedDeliver = "a test's deliveries (deliver) are not a list of case, after and sign: %w"

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
// HTTP answer's write), a sign: with an unknown scheme or without its header
// or secret_env, a case listed twice, or more than MaxDeliveriesPerTest
// entries.
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
		if err := validSign(d.Sign); err != "" {
			return refuse(DeliverSignInvalid, "%s's sign: %s", d.Case, err)
		}
	}
	return nil
}

// validSign is why s is not a sign: keploy can use ("" when it is, or nil).
func validSign(s *SignSpec) string {
	if s == nil {
		return ""
	}
	known := false
	for _, k := range SignSchemes {
		if s.Scheme == k {
			known = true
		}
	}
	switch {
	case !known:
		return fmt.Sprintf("scheme %q is not one keploy signs with (%v)", s.Scheme, SignSchemes)
	case s.Header == "":
		return "names no header to carry the signature"
	case s.SecretEnv == "":
		return "names no secret_env to read the secret from"
	}
	return ""
}
