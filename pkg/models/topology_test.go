package models

import (
	"encoding/json"
	"reflect"
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

// Every field of a mocked row reads back as it was written, in YAML and in
// JSON (the agent's wire format): the reader copies the row field by field
// (set), so a field added to MockedRow and not to that copy would read back
// empty with no compile error. Each field is filled through reflection, so a
// new one is filled too.
func TestEveryFieldOfAMockedRowRoundTrips(t *testing.T) {
	var row MockedRow
	v := reflect.ValueOf(&row).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("v" + v.Type().Field(i).Name)
		case reflect.Int, reflect.Int64, reflect.Int32:
			f.SetInt(int64(100 + i))
		case reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
			f.SetUint(uint64(200 + i))
		case reflect.Bool:
			f.SetBool(true)
		default:
			t.Fatalf("MockedRow.%s is a %s: teach this test to fill it", v.Type().Field(i).Name, f.Kind())
		}
	}
	y, err := yaml.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML MockedRow
	if err := yaml.Unmarshal(y, &fromYAML); err != nil {
		t.Fatal(err)
	}
	if fromYAML != row {
		t.Errorf("YAML read back %+v, want %+v (wrote %s)", fromYAML, row, y)
	}
	j, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON MockedRow
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatal(err)
	}
	if fromJSON != row {
		t.Errorf("JSON read back %+v, want %+v (wrote %s)", fromJSON, row, j)
	}
}
