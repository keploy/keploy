package golang

import (
	"debug/buildinfo"
	"runtime/debug"
	"testing"
)

func TestBuiltWithCover(t *testing.T) {
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     bool
	}{
		{"cover on", []debug.BuildSetting{{Key: "-trimpath", Value: "true"}, {Key: "-cover", Value: "true"}}, true},
		{"cover off", []debug.BuildSetting{{Key: "-cover", Value: "false"}}, false},
		{"no cover setting", []debug.BuildSetting{{Key: "-trimpath", Value: "true"}}, false},
		{"no settings", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := builtWithCover(&buildinfo.BuildInfo{Settings: tt.settings}); got != tt.want {
				t.Errorf("builtWithCover(%v) = %v, want %v", tt.settings, got, tt.want)
			}
		})
	}
}
