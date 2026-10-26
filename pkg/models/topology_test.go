package models

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A mocked row names its container by the key the mocks carry
// (containerKey: container:<repository>:<port>@<fingerprint>, as mocks.yaml
// writes it), so a reader who greps mocks.yaml for it finds them, and never
// as "container: container:...". A row an earlier keploy wrote under
// container: still reads.
func TestAMockedRowNamesItsContainerAsTheMocksDo(t *testing.T) {
	row := MockedRow{Test: "o::T", Addr: "[::1]:27019", Container: "container:mongo:27017@s1b008db297fc", Port: 27019}
	y, err := yaml.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(y), "containerKey: container:mongo:27017@s1b008db297fc\n") || strings.Contains(string(y), "\ncontainer: ") {
		t.Fatalf("written as:\n%s", y)
	}
	j, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(j), `"container":"container:mongo:27017@s1b008db297fc"`) {
		t.Fatalf("the agent's wire format changed the key an earlier CLI reads: written as %s", j)
	}
	for _, old := range []string{"addr: '[::1]:27019'\ncontainer: container:mongo:27017@s1b008db297fc\nport: 27019\n", "addr: '[::1]:27019'\ncontainerKey: container:mongo:27017@s1b008db297fc\nport: 27019\n"} {
		var back MockedRow
		if err := yaml.Unmarshal([]byte(old), &back); err != nil {
			t.Fatal(err)
		}
		if back.Container != row.Container {
			t.Fatalf("%q read as %+v", old, back)
		}
	}
	for _, j := range []string{`{"addr":"[::1]:27019","container":"container:mongo:27017@s1b008db297fc","port":27019}`, `{"addr":"[::1]:27019","containerKey":"container:mongo:27017@s1b008db297fc","port":27019}`} {
		var back MockedRow
		if err := json.Unmarshal([]byte(j), &back); err != nil || back.Container != row.Container {
			t.Fatalf("%s read as %+v, %v", j, back, err)
		}
	}
}
