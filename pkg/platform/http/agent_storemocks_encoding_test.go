package http

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcceptsEncoding(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   bool
	}{
		{"exact", []string{"zstd"}, true},
		{"in a list", []string{"gzip, zstd"}, true},
		{"across header lines", []string{"gzip", "zstd"}, true},
		{"case and spaces", []string{"  ZSTD  "}, true},
		{"with a weight", []string{"zstd;q=0.5"}, true},
		{"refused with q=0", []string{"zstd;q=0"}, false},
		{"refused with q=0.0 and spaces", []string{"gzip, zstd ; q=0.0"}, false},
		{"other codings only", []string{"gzip, br"}, false},
		{"a longer name", []string{"zstdx"}, false},
		{"no header", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, acceptsEncoding(tt.values, "zstd"))
		})
	}
}
