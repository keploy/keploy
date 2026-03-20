package http

import "testing"

func TestParseMediaTypes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"simple", "application/json", []string{"application/json"}},
		{"with charset", "application/json;charset=UTF-8", []string{"application/json"}},
		{"with charset and spaces", "application/json; charset=UTF-8", []string{"application/json"}},
		{"comma joined", "text/html, application/json", []string{"text/html", "application/json"}},
		{"comma joined with params", "text/html;charset=utf-8, application/json;charset=UTF-8", []string{"text/html", "application/json"}},
		{"empty", "", nil},
		{"malformed", "not a valid / media type ;;;", nil},
		{"one valid one malformed", "application/json, ;;;invalid", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMediaTypes(tt.input)
			if tt.want == nil {
				if got != nil {
					t.Errorf("parseMediaTypes(%q) = %v, want nil", tt.input, got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseMediaTypes(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("parseMediaTypes(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestMediaTypesOverlap(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want bool
	}{
		{"exact match", []string{"application/json"}, []string{"application/json"}, true},
		{"case insensitive", []string{"Application/JSON"}, []string{"application/json"}, true},
		{"no overlap", []string{"text/html"}, []string{"application/json"}, false},
		{"partial overlap", []string{"text/html", "application/json"}, []string{"application/json"}, true},
		{"empty a", nil, []string{"application/json"}, false},
		{"empty b", []string{"application/json"}, nil, false},
		{"both empty", nil, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mediaTypesOverlap(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("mediaTypesOverlap(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
