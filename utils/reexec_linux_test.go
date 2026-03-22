//go:build linux

package utils

import "testing"

func TestIsCloudReplayCmd(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{
			name: "cloud replay",
			args: []string{"keploy", "cloud", "replay"},
			want: true,
		},
		{
			name: "cloud replay with flags",
			args: []string{"keploy", "cloud", "replay", "--verbose"},
			want: true,
		},
		{
			name: "cloud without replay",
			args: []string{"keploy", "cloud", "record"},
			want: false,
		},
		{
			name: "only cloud",
			args: []string{"keploy", "cloud"},
			want: false,
		},
		{
			name: "replay without cloud",
			args: []string{"keploy", "replay"},
			want: false,
		},
		{
			name: "no subcommand",
			args: []string{"keploy"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCloudReplayCmd(tt.args); got != tt.want {
				t.Errorf("isCloudReplayCmd(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
