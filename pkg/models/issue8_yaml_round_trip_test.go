package models

import (
	"bytes"
	"net"
	"net/netip"
	"reflect"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestNetipPrefixCellRoundTrip pins the YAML round-trip for a
// netip.Prefix carried in a PostgresV3Cell. Released keploy emits
// netip.Prefix via reflection; netip.Prefix implements
// encoding.TextMarshaler so yaml.v3 produces a plain string scalar
// like "192.168.1.0/24". Without explicit cell-level dispatch the
// decode side resolves the untagged scalar back to `string`, not
// netip.Prefix — which means at replay time the integrations codec
// hits "cannot find encode plan" for `inet` / `cidr` columns (the
// pgtype dispatch keys off the Go type, not the textual form).
//
// This test pins the contract: the value must come back as
// netip.Prefix, not string and not map[string]any.
func TestNetipPrefixCellRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   netip.Prefix
	}{
		{"ipv4_24", netip.MustParsePrefix("192.168.1.0/24")},
		{"ipv4_host", netip.MustParsePrefix("10.0.0.1/32")},
		{"ipv6_32", netip.MustParsePrefix("2001:db8::/32")},
		{"ipv6_host", netip.MustParsePrefix("::1/128")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := PostgresV3Cells{NewValueCell(tc.in)}
			body, err := yaml.Marshal(row)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// The serialized form must NOT carry an `!pg/<name>` tag
			// (cross-version compat with released keploy on Docker
			// Hub, which doesn't register the custom tag set).
			if bytes.Contains(body, []byte("!pg/")) {
				t.Errorf("emitted YAML carries !pg/ tag (breaks cross-version replay):\n%s", body)
			}
			var out PostgresV3Cells
			if err := yaml.Unmarshal(body, &out); err != nil {
				t.Fatalf("unmarshal: %v\n--YAML--\n%s", err, body)
			}
			if len(out) != 1 {
				t.Fatalf("expected 1 cell, got %d", len(out))
			}
			got, ok := out[0].Value.(netip.Prefix)
			if !ok {
				t.Fatalf("Value is %T, want netip.Prefix\n--YAML--\n%s", out[0].Value, body)
			}
			if !reflect.DeepEqual(got, tc.in) {
				t.Errorf("round-trip drift:\n got  %v\n want %v\n--YAML--\n%s", got, tc.in, body)
			}
		})
	}
}

// TestHardwareAddrCellRoundTrip pins the YAML round-trip for
// net.HardwareAddr (PG `macaddr` / `macaddr8`). HardwareAddr is a
// named `[]byte`; the cell-level `case []byte:` only matches the
// unnamed type, so HardwareAddr falls through to the default
// `return c.Value, nil` branch, where yaml.v3's reflective encoder
// treats it as a generic `[]byte` and emits a sequence-of-ints
// (`- 1\n- 2\n...`). On reload the cell would come back as
// `[]any{int64(1), ...}` instead of net.HardwareAddr — the pgtype
// codec then can't find an encode plan for `macaddr`.
//
// This test pins the contract: the value must come back as
// net.HardwareAddr.
func TestHardwareAddrCellRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"mac6", "01:02:03:04:05:06"},
		{"mac8", "01:02:03:04:05:06:07:08"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mac, err := net.ParseMAC(tc.in)
			if err != nil {
				t.Fatalf("ParseMAC: %v", err)
			}
			row := PostgresV3Cells{NewValueCell(mac)}
			body, err := yaml.Marshal(row)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if bytes.Contains(body, []byte("!pg/")) {
				t.Errorf("emitted YAML carries !pg/ tag (breaks cross-version replay):\n%s", body)
			}
			var out PostgresV3Cells
			if err := yaml.Unmarshal(body, &out); err != nil {
				t.Fatalf("unmarshal: %v\n--YAML--\n%s", err, body)
			}
			if len(out) != 1 {
				t.Fatalf("expected 1 cell, got %d", len(out))
			}
			got, ok := out[0].Value.(net.HardwareAddr)
			if !ok {
				t.Fatalf("Value is %T, want net.HardwareAddr\n--YAML--\n%s", out[0].Value, body)
			}
			if !reflect.DeepEqual(got, mac) {
				t.Errorf("round-trip drift:\n got  %v\n want %v\n--YAML--\n%s", got, mac, body)
			}
		})
	}
}
