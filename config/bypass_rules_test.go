package config

import (
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// Only the port-only bypassRules go to the kernel, which passes every call to
// their ports through. A rule with a host or a path names some of a port's
// calls; its port in the kernel's list passed every host's calls on it through.
func TestOnlyPortOnlyBypassRulesGoToTheKernel(t *testing.T) {
	conf := &Config{BypassRules: []models.BypassRule{
		{Host: "api.example.com", Port: 443},
		{Port: 7003},
		{Path: "/v1/health", Port: 8080},
		{Host: "auth.example.com"},
		{Host: "x.example.com", Path: "/a", Port: 9000},
		{Port: 7004},
	}}
	if got, want := GetByPassPorts(conf), []uint{7003, 7004}; !slices.Equal(got, want) {
		t.Fatalf("GetByPassPorts = %v, want %v (the port-only rules' ports)", got, want)
	}
	SetByPassPorts(conf, []uint{7005})
	if got, want := GetByPassPorts(conf), []uint{7003, 7004, 7005}; !slices.Equal(got, want) {
		t.Fatalf("after the flag's port, GetByPassPorts = %v, want %v", got, want)
	}
	if len(conf.BypassRules) != 7 {
		t.Fatalf("the rules with a host or a path must stay for the proxy: %+v", conf.BypassRules)
	}
}

// A rule with a host or a path on a mysqlPorts port can match none of the
// port's calls (MySQL shows no host or path), so keploy refuses it at start,
// naming it and the port-only rule to use instead.
func TestBypassRulesKeployCannotApplyAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mysql   []uint32
		rules   []models.BypassRule
		refused []string
	}{
		{"no rules", []uint32{3307}, nil, nil},
		{"port-only rule on a mysql port", []uint32{3307}, []models.BypassRule{{Port: 3307}}, nil},
		{"host rule on another port", []uint32{3307}, []models.BypassRule{{Host: "db.internal", Port: 5432}}, nil},
		{"host rule, no mysqlPorts", nil, []models.BypassRule{{Host: "db.internal", Port: 3306}}, nil},
		{"host rule on any port", []uint32{3307}, []models.BypassRule{{Host: "db.internal"}}, nil},
		{"host rule on a mysql port", []uint32{3307}, []models.BypassRule{{Host: "db.internal", Port: 3307}}, []string{`{host: "db.internal", path: "", port: 3307}`}},
		{"path rule on a mysql port", []uint32{3306, 3307}, []models.BypassRule{{Port: 3306}, {Path: "/x", Port: 3306}}, []string{`{host: "", path: "/x", port: 3306}`}},
		{"two refused", []uint32{3306, 3307}, []models.BypassRule{{Host: "a", Port: 3306}, {Host: "b", Port: 3307}}, []string{`{host: "a", path: "", port: 3306}`, `{host: "b", path: "", port: 3307}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckBypassRules(&Config{MysqlPorts: tc.mysql, BypassRules: tc.rules})
			if len(tc.refused) == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("not refused; want %v named", tc.refused)
			}
			for _, r := range append(tc.refused, "{port: N}", "mysqlPorts") {
				if !strings.Contains(err.Error(), r) {
					t.Errorf("the error does not name %s: %v", r, err)
				}
			}
		})
	}
}
