package utils

import (
	"bytes"
	"testing"
)

func TestParseOutputOfLSOF(t *testing.T) {
	type args struct {
		cmdStdOut *bytes.Buffer
		cmdStdErr *bytes.Buffer
	}
	tests := []struct {
		name    string
		args    args
		want    uint32
		wantErr bool
	}{
		{
			name: "lsof missing from OS",
			args: args{
				cmdStdOut: new(bytes.Buffer),
				cmdStdErr: bytes.NewBufferString("failed to run lsof command"),
			},
			want:    0,
			wantErr: true,
		},
		{
			name: "more than one pid are returned for a specific port",
			args: args{
				cmdStdOut: bytes.NewBufferString(`COMMAND  PID      USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
							main    5044 aerowisca    3u  IPv6  73427      0t0  TCP *:tproxy (LISTEN)
							main    5045 aerowisca    3u  IPv6  73427      0t0  TCP *:tproxy (LISTEN)`),
				cmdStdErr: new(bytes.Buffer),
			},
			want:    0,
			wantErr: true,
		},
		{
			name: "zero pid is returned for a specific port",
			args: args{
				cmdStdOut: bytes.NewBufferString(`COMMAND  PID      USER   FD   TYPE DEVICE SIZE/OFF NODE NAME`),
				cmdStdErr: new(bytes.Buffer),
			},
			want:    0,
			wantErr: true,
		},
		{
			name: "pid is absent",
			args: args{
				cmdStdOut: bytes.NewBufferString(`COMMAND  PID      USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
							main`),
				cmdStdErr: new(bytes.Buffer),
			},
			want:    0,
			wantErr: true,
		},
		{
			name: "the pid is not an integer",
			args: args{
				cmdStdOut: bytes.NewBufferString(`COMMAND  PID      USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
							main    fakePID aerowisca    3u  IPv6  73427      0t0  TCP *:tproxy (LISTEN)`),
				cmdStdErr: new(bytes.Buffer),
			},
			want:    0,
			wantErr: true,
		},
		{
			name: "all fields are populated correctly",
			args: args{
				cmdStdOut: bytes.NewBufferString(`COMMAND  PID      USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
							main    5044 aerowisca    3u  IPv6  73427      0t0  TCP *:tproxy (LISTEN)`),
				cmdStdErr: new(bytes.Buffer),
			},
			want:    5044,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOutputOfLSOF(tt.args.cmdStdOut, tt.args.cmdStdErr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseOutputOfLSOF() error = %+v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseOutputOfLSOF() got = %v, want %v", got, tt.want)
			}
		})
	}
}
