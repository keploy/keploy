package models

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

const deliverEntry = `id: pay::TestPaidByCallback
mock_entries:
  - name: mock-3
    kind: Http
  - name: mock-4
    kind: Postgres
cases: [test-1, test-2]
deliver:
  - case: test-2
    after: mock-3
`

// Under the switch a test's deliver: is read and written as it was, in YAML
// and in JSON; with the switch off it is not read.
func TestDeliverRoundTrips(t *testing.T) {
	want := []Delivery{{Case: "test-2", After: "mock-3"}}
	t.Setenv(provenswitch.Env, "1")
	var tc MappedTestCase
	require.NoError(t, yaml.Unmarshal([]byte(deliverEntry), &tc))
	require.Equal(t, want, tc.Deliver)

	out, err := yaml.Marshal(tc)
	require.NoError(t, err)
	var back MappedTestCase
	require.NoError(t, yaml.Unmarshal(out, &back))
	require.Equal(t, want, back.Deliver, string(out))

	js, err := json.Marshal(tc)
	require.NoError(t, err)
	var fromJSON MappedTestCase
	require.NoError(t, json.Unmarshal(js, &fromJSON))
	require.Equal(t, want, fromJSON.Deliver, string(js))

	none, err := yaml.Marshal(MappedTestCase{ID: "t"})
	require.NoError(t, err)
	require.NotContains(t, string(none), "deliver", "a test with no delivery is written as before")

	t.Setenv(provenswitch.Env, "")
	var off MappedTestCase
	require.NoError(t, yaml.Unmarshal([]byte(deliverEntry), &off))
	require.Nil(t, off.Deliver)
	var offJSON MappedTestCase
	require.NoError(t, json.Unmarshal(js, &offJSON))
	require.Nil(t, offJSON.Deliver)
}

// A deliver: keploy could not send as written refuses the test's entry, with
// its code and a message that names the fix; in YAML and in JSON.
func TestDeliverRefusesWhatItCouldNotSend(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	entry := func(deliver string) string {
		return strings.Replace(deliverEntry, deliverEntry[strings.Index(deliverEntry, "deliver:"):], deliver, 1)
	}
	many := "deliver:\n"
	for i := 0; i <= MaxDeliveriesPerTest; i++ {
		many += "  - {case: test-2, after: mock-3}\n"
	}
	for _, c := range []struct {
		name, deliver, code, msg string
	}{
		{"case not the test's", "deliver: [{case: test-9, after: mock-3}]", DeliverCaseUnknown,
			`case "test-9" is not one of this test's cases; name a case listed under its cases:`},
		{"after not the test's", "deliver: [{case: test-2, after: mock-9}]", DeliverAfterUnknown,
			`test-2's after: "mock-9" is not one of this test's mock_entries; set after: to the call that triggers it`},
		{"after not HTTP", "deliver: [{case: test-2, after: mock-4}]", DeliverAfterUnknown,
			`test-2's after: "mock-4" is not an HTTP call (kind "Postgres"); keploy delivers after an HTTP call's answer only`},
		{"case twice", "deliver: [{case: test-2, after: mock-3}, {case: test-2, after: mock-3}]", DeliverCaseTwice,
			`case "test-2" is listed twice; keploy delivers a case once`},
		{"sign: named", "deliver: [{case: test-2, after: mock-3, sign: {scheme: hmac-sha256-hex, header: X-S, secret_env: S}}]", DeliverSignUnsupported,
			"test-2's sign: is not supported yet; remove it, or keep the body as recorded"},
		{"sign: empty", "deliver: [{case: test-2, after: mock-3, sign: {}}]", DeliverSignUnsupported,
			"test-2's sign: is not supported yet; remove it, or keep the body as recorded"},
		{"sign: a word", "deliver: [{case: test-2, after: mock-3, sign: hmac}]", DeliverSignUnsupported,
			"test-2's sign: is not supported yet; remove it, or keep the body as recorded"},
		{"too many", many, DeliverTooMany,
			"65 entries, more than the 64 keploy delivers in one test"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var tc MappedTestCase
			err := yaml.Unmarshal([]byte(entry(c.deliver)), &tc)
			var de *DeliverError
			require.True(t, errors.As(err, &de), "%v", err)
			require.Equal(t, c.code, de.Code)
			require.Equal(t, `test "pay::TestPaidByCallback" in mappings: deliver: `+c.msg, err.Error())

			var y map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(entry(c.deliver)), &y))
			js, jerr := json.Marshal(y)
			require.NoError(t, jerr)
			var fromJSON MappedTestCase
			err = json.Unmarshal(js, &fromJSON)
			require.True(t, errors.As(err, &de), "%v", err)
			require.Equal(t, c.code, de.Code)
		})
	}
	// An entry with no sign: (or a null one) is taken, as is the most a
	// test may have.
	for _, d := range []string{"deliver: [{case: test-2, after: mock-3}]", "deliver: [{case: test-2, after: mock-3, sign: null}]"} {
		var unsigned MappedTestCase
		require.NoError(t, yaml.Unmarshal([]byte(entry(d)), &unsigned), d)
		require.Nil(t, unsigned.Deliver[0].Sign)
	}
	most := MappedTestCase{ID: "t", Mocks: []MockEntry{{Name: "mock-3", Kind: string(HTTP)}}}
	for i := 0; i < MaxDeliveriesPerTest; i++ {
		c := "test-" + strconv.Itoa(i)
		most.Cases = append(most.Cases, c)
		most.Deliver = append(most.Deliver, Delivery{Case: c, After: "mock-3"})
	}
	require.NoError(t, ValidateDeliver(most))
}

// A deliver: that is not a list of entries is an error, never a reason to
// read the test without it.
func TestDeliverThatIsNotAListIsAnError(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	bad := strings.Replace(deliverEntry, deliverEntry[strings.Index(deliverEntry, "deliver:"):], "deliver: test-2\n", 1)
	var tc MappedTestCase
	err := yaml.Unmarshal([]byte(bad), &tc)
	require.ErrorContains(t, err, "a test's deliveries (deliver) are not a list of case and after")
	err = json.Unmarshal([]byte(`{"id":"t","mock_entries":[],"deliver":"test-2"}`), &tc)
	require.ErrorContains(t, err, "a test's deliveries (deliver) are not a list of case and after")
}

// The table carries a delivery's entry beside its case's request and answer.
func TestDeliveryCaseWire(t *testing.T) {
	dc := DeliveryCase{Delivery: Delivery{Case: "test-2", After: "mock-3"}, Req: HTTPReq{Method: "POST", Body: `{"a":1}`}, Resp: HTTPResp{StatusCode: 200}}
	js, err := json.Marshal(dc)
	require.NoError(t, err)
	require.Contains(t, string(js), `"case":"test-2","after":"mock-3","req":`)
	var back DeliveryCase
	require.NoError(t, json.Unmarshal(js, &back))
	require.Equal(t, dc.Delivery, back.Delivery)
	require.Equal(t, dc.Req.Body, back.Req.Body)
	require.Equal(t, 200, back.Resp.StatusCode)
}
